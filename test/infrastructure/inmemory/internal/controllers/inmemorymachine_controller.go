/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package controllers implements controller functionality.
package controllers

import (
	"context"
	"crypto/rsa"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	infrav1 "sigs.k8s.io/cluster-api/test/infrastructure/inmemory/api/v1alpha1"
	"sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/cloud"
	cloudv1 "sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/cloud/api/v1alpha1"
	"sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/server"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/certs"
	"sigs.k8s.io/cluster-api/util/conditions"
	clog "sigs.k8s.io/cluster-api/util/log"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	"sigs.k8s.io/cluster-api/util/secret"
)

// InMemoryMachineReconciler reconciles a InMemoryMachine object.
type InMemoryMachineReconciler struct {
	client.Client
	CloudManager cloud.Manager
	APIServerMux *server.WorkloadClustersMux

	// WatchFilterValue is the label value used to filter events prior to reconciliation.
	WatchFilterValue string
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=inmemorymachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=inmemorymachines/status;inmemorymachines/finalizers,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;machinesets;machines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile handles InMemoryMachine events.
func (r *InMemoryMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, rerr error) {
	log := ctrl.LoggerFrom(ctx)

	// Fetch the InMemoryMachine instance
	inMemoryMachine := &infrav1.InMemoryMachine{}
	if err := r.Client.Get(ctx, req.NamespacedName, inMemoryMachine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// AddOwners adds the owners of InMemoryMachine as k/v pairs to the logger.
	// Specifically, it will add KubeadmControlPlane, MachineSet and MachineDeployment.
	ctx, log, err := clog.AddOwners(ctx, r.Client, inMemoryMachine)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Fetch the Machine.
	machine, err := util.GetOwnerMachine(ctx, r.Client, inMemoryMachine.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("Waiting for Machine Controller to set OwnerRef on InMemoryMachine")
		return ctrl.Result{}, nil
	}

	log = log.WithValues("Machine", klog.KObj(machine))
	ctx = ctrl.LoggerInto(ctx, log)

	// Fetch the Cluster.
	cluster, err := util.GetClusterFromMetadata(ctx, r.Client, machine.ObjectMeta)
	if err != nil {
		log.Info("InMemoryMachine owner Machine is missing cluster label or cluster does not exist")
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info(fmt.Sprintf("Please associate this machine with a cluster using the label %s: <name of cluster>", clusterv1.ClusterNameLabel))
		return ctrl.Result{}, nil
	}

	log = log.WithValues("Cluster", klog.KObj(cluster))
	ctx = ctrl.LoggerInto(ctx, log)

	// Return early if the object or Cluster is paused.
	if annotations.IsPaused(cluster, inMemoryMachine) {
		log.Info("Reconciliation is paused for this object")
		return ctrl.Result{}, nil
	}

	// Fetch the in-memory Cluster.
	inMemoryCluster := &infrav1.InMemoryCluster{}
	inMemoryClusterName := client.ObjectKey{
		Namespace: inMemoryMachine.Namespace,
		Name:      cluster.Spec.InfrastructureRef.Name,
	}
	if err := r.Client.Get(ctx, inMemoryClusterName, inMemoryCluster); err != nil {
		log.Info("InMemoryCluster is not available yet")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(inMemoryMachine, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to Patch the InMemoryMachine object and status after each reconciliation.
	defer func() {
		inMemoryMachineConditions := []clusterv1.ConditionType{
			infrav1.VMProvisionedCondition,
			infrav1.NodeProvisionedCondition,
		}
		if util.IsControlPlaneMachine(machine) {
			inMemoryMachineConditions = append(inMemoryMachineConditions,
				infrav1.EtcdProvisionedCondition,
				infrav1.APIServerProvisionedCondition,
			)
		}
		// Always update the readyCondition by summarizing the state of other conditions.
		// A step counter is added to represent progress during the provisioning process (instead we are hiding the step counter during the deletion process).
		conditions.SetSummary(inMemoryMachine,
			conditions.WithConditions(inMemoryMachineConditions...),
			conditions.WithStepCounterIf(inMemoryMachine.ObjectMeta.DeletionTimestamp.IsZero() && inMemoryMachine.Spec.ProviderID == nil),
		)
		if err := patchHelper.Patch(ctx, inMemoryMachine, patch.WithOwnedConditions{Conditions: inMemoryMachineConditions}); err != nil {
			log.Error(err, "failed to patch InMemoryMachine")
			if rerr == nil {
				rerr = err
			}
		}
	}()

	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(inMemoryMachine, infrav1.MachineFinalizer) {
		controllerutil.AddFinalizer(inMemoryMachine, infrav1.MachineFinalizer)
		return ctrl.Result{}, nil
	}

	// Handle deleted machines
	if !inMemoryMachine.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, cluster, machine, inMemoryMachine)
	}

	// Handle non-deleted machines
	return r.reconcileNormal(ctx, cluster, machine, inMemoryMachine)
}

func (r *InMemoryMachineReconciler) reconcileNormal(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// Check if the infrastructure is ready, otherwise return and wait for the cluster object to be updated
	if !cluster.Status.InfrastructureReady {
		conditions.MarkFalse(inMemoryMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForClusterInfrastructureReason, clusterv1.ConditionSeverityInfo, "")
		log.Info("Waiting for InMemoryCluster Controller to create cluster infrastructure")
		return ctrl.Result{}, nil
	}

	// Make sure bootstrap data is available and populated.
	// NOTE: we are not using bootstrap data, but we wait for it in order to simulate a real machine
	// provisioning workflow.
	if machine.Spec.Bootstrap.DataSecretName == nil {
		if !util.IsControlPlaneMachine(machine) && !conditions.IsTrue(cluster, clusterv1.ControlPlaneInitializedCondition) {
			conditions.MarkFalse(inMemoryMachine, infrav1.VMProvisionedCondition, infrav1.WaitingControlPlaneInitializedReason, clusterv1.ConditionSeverityInfo, "")
			log.Info("Waiting for the control plane to be initialized")
			return ctrl.Result{}, nil
		}

		conditions.MarkFalse(inMemoryMachine, infrav1.VMProvisionedCondition, infrav1.WaitingForBootstrapDataReason, clusterv1.ConditionSeverityInfo, "")
		log.Info("Waiting for the Bootstrap provider controller to set bootstrap data")
		return ctrl.Result{}, nil
	}

	// Call the inner reconciliation methods.
	phases := []func(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error){
		r.reconcileNormalCloudMachine,
		r.reconcileNormalNode,
		r.reconcileNormalETCD,
		r.reconcileNormalAPIServer,
		r.reconcileNormalScheduler,
		r.reconcileNormalControllerManager,
		r.reconcileNormalKubeadmObjects,
	}

	res := ctrl.Result{}
	errs := []error{}
	for _, phase := range phases {
		phaseResult, err := phase(ctx, cluster, machine, inMemoryMachine)
		if err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			continue
		}
		// TODO: consider if we have to use max(RequeueAfter) instead of min(RequeueAfter) to reduce the pressure on
		//  the reconcile queue for InMemoryMachines given that we are requeuing just to wait for some period to expire;
		//  the downside of it is that InMemoryMachines status will change by "big steps" vs incrementally.
		res = util.LowestNonZeroResult(res, phaseResult)
	}
	return res, kerrors.NewAggregate(errs)
}

func (r *InMemoryMachineReconciler) reconcileNormalCloudMachine(ctx context.Context, cluster *clusterv1.Cluster, _ *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create VM; a Cloud VM can be created as soon as the Infra Machine is created
	// NOTE: for sake of simplicity we keep cloud resources as global resources (namespace empty).
	cloudMachine := &cloudv1.CloudMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: inMemoryMachine.Name,
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(cloudMachine), cloudMachine); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		if err := cloudClient.Create(ctx, cloudMachine); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create CloudMachine")
		}
	}

	// Wait for the VM to be provisioned; provisioned happens a configurable time after the cloud machine creation.
	provisioningDuration := time.Duration(0)
	if inMemoryMachine.Spec.Behaviour != nil && inMemoryMachine.Spec.Behaviour.VM != nil {
		x := inMemoryMachine.Spec.Behaviour.VM.Provisioning

		provisioningDuration = x.StartupDuration.Duration
		jitter, err := strconv.ParseFloat(x.StartupJitter, 64)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to parse VM's StartupJitter")
		}
		if jitter > 0.0 {
			provisioningDuration += time.Duration(rand.Float64() * jitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.
		}
	}

	start := cloudMachine.CreationTimestamp
	now := time.Now()
	if now.Before(start.Add(provisioningDuration)) {
		conditions.MarkFalse(inMemoryMachine, infrav1.VMProvisionedCondition, infrav1.VMWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
	}

	// TODO: consider if to surface VM provisioned also on the cloud machine (currently it surfaces only on the inMemoryMachine)

	conditions.MarkTrue(inMemoryMachine, infrav1.VMProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalNode(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the VM is not provisioned yet
	if !conditions.IsTrue(inMemoryMachine, infrav1.VMProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Wait for the node/kubelet to start up; node/kubelet start happens a configurable time after the VM is provisioned.
	provisioningDuration := time.Duration(0)
	if inMemoryMachine.Spec.Behaviour != nil && inMemoryMachine.Spec.Behaviour.Node != nil {
		x := inMemoryMachine.Spec.Behaviour.Node.Provisioning

		provisioningDuration = x.StartupDuration.Duration
		jitter, err := strconv.ParseFloat(x.StartupJitter, 64)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to parse node's StartupJitter")
		}
		if jitter > 0.0 {
			provisioningDuration += time.Duration(rand.Float64() * jitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.
		}
	}

	start := conditions.Get(inMemoryMachine, infrav1.VMProvisionedCondition).LastTransitionTime
	now := time.Now()
	if now.Before(start.Add(provisioningDuration)) {
		conditions.MarkFalse(inMemoryMachine, infrav1.NodeProvisionedCondition, infrav1.NodeWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create Node
	// TODO: consider if to handle an additional setting adding a delay in between create node and node ready/provider ID being set
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: inMemoryMachine.Name,
		},
		Spec: corev1.NodeSpec{
			ProviderID: fmt.Sprintf("in-memory://%s", inMemoryMachine.Name),
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:   corev1.NodeReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if util.IsControlPlaneMachine(machine) {
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels["node-role.kubernetes.io/control-plane"] = ""
	}

	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(node), node); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get node")
		}

		// NOTE: for the first control plane machine we might create the node before etcd and API server pod are running
		// but this is not an issue, because it won't be visible to CAPI until the API server start serving requests.
		if err := cloudClient.Create(ctx, node); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create Node")
		}
	}

	inMemoryMachine.Spec.ProviderID = &node.Spec.ProviderID
	inMemoryMachine.Status.Ready = true

	conditions.MarkTrue(inMemoryMachine, infrav1.NodeProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalETCD(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// No-op if the Node is not provisioned yet
	if !conditions.IsTrue(inMemoryMachine, infrav1.NodeProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Wait for the etcd pod to start up; etcd pod start happens a configurable time after the Node is provisioned.
	provisioningDuration := time.Duration(0)
	if inMemoryMachine.Spec.Behaviour != nil && inMemoryMachine.Spec.Behaviour.Etcd != nil {
		x := inMemoryMachine.Spec.Behaviour.Etcd.Provisioning

		provisioningDuration = x.StartupDuration.Duration
		jitter, err := strconv.ParseFloat(x.StartupJitter, 64)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to parse etcd's StartupJitter")
		}
		if jitter > 0.0 {
			provisioningDuration += time.Duration(rand.Float64() * jitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.
		}
	}

	start := conditions.Get(inMemoryMachine, infrav1.NodeProvisionedCondition).LastTransitionTime
	now := time.Now()
	if now.Before(start.Add(provisioningDuration)) {
		conditions.MarkFalse(inMemoryMachine, infrav1.EtcdProvisionedCondition, infrav1.EtcdWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the etcd pod
	// TODO: consider if to handle an additional setting adding a delay in between create pod and pod ready
	etcdMember := fmt.Sprintf("etcd-%s", inMemoryMachine.Name)
	etcdPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      etcdMember,
			Labels: map[string]string{
				"component": "etcd",
				"tier":      "control-plane",
			},
			Annotations: map[string]string{
				// TODO: read this from existing etcd pods, if any, otherwise all the member will get a different ClusterID.
				"etcd.inmemory.infrastructure.cluster.x-k8s.io/cluster-id": fmt.Sprintf("%d", rand.Uint32()), //nolint:gosec // weak random number generator is good enough here
				"etcd.inmemory.infrastructure.cluster.x-k8s.io/member-id":  fmt.Sprintf("%d", rand.Uint32()), //nolint:gosec // weak random number generator is good enough here
				// TODO: set this only if there are no other leaders.
				"etcd.inmemory.infrastructure.cluster.x-k8s.io/leader-from": time.Now().Format(time.RFC3339),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(etcdPod), etcdPod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get etcd Pod")
		}

		// NOTE: for the first control plane machine we might create the etcd pod before the API server pod is running
		// but this is not an issue, because it won't be visible to CAPI until the API server start serving requests.
		if err := cloudClient.Create(ctx, etcdPod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create Pod")
		}
	}

	// If there is not yet an etcd member listener for this machine, add it to the server.
	if !r.APIServerMux.HasEtcdMember(resourceGroup, etcdMember) {
		// Getting the etcd CA
		s, err := secret.Get(ctx, r.Client, client.ObjectKeyFromObject(cluster), secret.EtcdCA)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get etcd CA")
		}
		certData, exists := s.Data[secret.TLSCrtDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid etcd CA: missing data for %s", secret.TLSCrtDataName)
		}

		cert, err := certs.DecodeCertPEM(certData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid etcd CA: invalid %s", secret.TLSCrtDataName)
		}

		keyData, exists := s.Data[secret.TLSKeyDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid etcd CA: missing data for %s", secret.TLSKeyDataName)
		}

		key, err := certs.DecodePrivateKeyPEM(keyData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid etcd CA: invalid %s", secret.TLSKeyDataName)
		}

		if err := r.APIServerMux.AddEtcdMember(resourceGroup, etcdMember, cert, key.(*rsa.PrivateKey)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to start etcd member")
		}
	}

	conditions.MarkTrue(inMemoryMachine, infrav1.EtcdProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalAPIServer(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// No-op if the Node is not provisioned yet
	if !conditions.IsTrue(inMemoryMachine, infrav1.NodeProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Wait for the API server pod to start up; API server pod start happens a configurable time after the Node is provisioned.
	provisioningDuration := time.Duration(0)
	if inMemoryMachine.Spec.Behaviour != nil && inMemoryMachine.Spec.Behaviour.APIServer != nil {
		x := inMemoryMachine.Spec.Behaviour.APIServer.Provisioning

		provisioningDuration = x.StartupDuration.Duration
		jitter, err := strconv.ParseFloat(x.StartupJitter, 64)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to parse API server's StartupJitter")
		}
		if jitter > 0.0 {
			provisioningDuration += time.Duration(rand.Float64() * jitter * float64(provisioningDuration)) //nolint:gosec // Intentionally using a weak random number generator here.
		}
	}

	start := conditions.Get(inMemoryMachine, infrav1.NodeProvisionedCondition).LastTransitionTime
	now := time.Now()
	if now.Before(start.Add(provisioningDuration)) {
		conditions.MarkFalse(inMemoryMachine, infrav1.APIServerProvisionedCondition, infrav1.APIServerWaitingForStartupTimeoutReason, clusterv1.ConditionSeverityInfo, "")
		return ctrl.Result{RequeueAfter: start.Add(provisioningDuration).Sub(now)}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Create the apiserver pod
	// TODO: consider if to handle an additional setting adding a delay in between create pod and pod ready
	apiServer := fmt.Sprintf("kube-apiserver-%s", inMemoryMachine.Name)

	apiServerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      apiServer,
			Labels: map[string]string{
				"component": "kube-apiserver",
				"tier":      "control-plane",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Get(ctx, client.ObjectKeyFromObject(apiServerPod), apiServerPod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get apiServer Pod")
		}

		if err := cloudClient.Create(ctx, apiServerPod); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, errors.Wrapf(err, "failed to create apiServer Pod")
		}
	}

	// If there is not yet an API server listener for this machine.
	if !r.APIServerMux.HasAPIServer(resourceGroup, apiServer) {
		// Getting the Kubernetes CA
		s, err := secret.Get(ctx, r.Client, client.ObjectKeyFromObject(cluster), secret.ClusterCA)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get cluster CA")
		}
		certData, exists := s.Data[secret.TLSCrtDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid cluster CA: missing data for %s", secret.TLSCrtDataName)
		}

		cert, err := certs.DecodeCertPEM(certData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid cluster CA: invalid %s", secret.TLSCrtDataName)
		}

		keyData, exists := s.Data[secret.TLSKeyDataName]
		if !exists {
			return ctrl.Result{}, errors.Errorf("invalid cluster CA: missing data for %s", secret.TLSKeyDataName)
		}

		key, err := certs.DecodePrivateKeyPEM(keyData)
		if err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "invalid cluster CA: invalid %s", secret.TLSKeyDataName)
		}

		// Adding the APIServer.
		// NOTE: When the first APIServer is added, the workload cluster listener is started.
		if err := r.APIServerMux.AddAPIServer(resourceGroup, apiServer, cert, key.(*rsa.PrivateKey)); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to start API server")
		}
	}

	conditions.MarkTrue(inMemoryMachine, infrav1.APIServerProvisionedCondition)
	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalScheduler(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// NOTE: we are creating the scheduler pod to make KCP happy, but we are not implementing any
	// specific behaviour for this component because they are not relevant for stress tests.
	// As a current approximation, we create the scheduler as soon as the API server is provisioned;
	// also, the scheduler is immediately marked as ready.
	if !conditions.IsTrue(inMemoryMachine, infrav1.APIServerProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	schedulerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-scheduler-%s", inMemoryMachine.Name),
			Labels: map[string]string{
				"component": "kube-scheduler",
				"tier":      "control-plane",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Create(ctx, schedulerPod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create scheduler Pod")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalControllerManager(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// NOTE: we are creating the controller manager pod to make KCP happy, but we are not implementing any
	// specific behaviour for this component because they are not relevant for stress tests.
	// As a current approximation, we create the controller manager as soon as the API server is provisioned;
	// also, the controller manager is immediately marked as ready.
	if !conditions.IsTrue(inMemoryMachine, infrav1.APIServerProvisionedCondition) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	controllerManagerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-controller-manager-%s", inMemoryMachine.Name),
			Labels: map[string]string{
				"component": "kube-controller-manager",
				"tier":      "control-plane",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}
	if err := cloudClient.Create(ctx, controllerManagerPod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create controller manager Pod")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileNormalKubeadmObjects(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, _ *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// create kubeadm ClusterRole and ClusterRoleBinding enforced by KCP
	// NOTE: we create those objects because this is what kubeadm does, but KCP creates
	// ClusterRole and ClusterRoleBinding if not found.

	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeadm:get-nodes",
		},
		Rules: []rbacv1.PolicyRule{
			{
				Verbs:     []string{"get"},
				APIGroups: []string{""},
				Resources: []string{"nodes"},
			},
		},
	}
	if err := cloudClient.Create(ctx, role); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create kubeadm:get-nodes ClusterRole")
	}

	roleBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "kubeadm:get-nodes",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     "kubeadm:get-nodes",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind: rbacv1.GroupKind,
				Name: "system:bootstrappers:kubeadm:default-node-token",
			},
		},
	}
	if err := cloudClient.Create(ctx, roleBinding); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create kubeadm:get-nodes ClusterRoleBinding")
	}

	// create kubeadm config map
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kubeadm-config",
			Namespace: metav1.NamespaceSystem,
		},
	}
	if err := cloudClient.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to create ubeadm-config ConfigMap")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDelete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// Call the inner reconciliation methods.
	phases := []func(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error){
		// TODO: revisit order when we implement behaviour for the deletion workflow
		r.reconcileDeleteNode,
		r.reconcileDeleteETCD,
		r.reconcileDeleteAPIServer,
		r.reconcileDeleteScheduler,
		r.reconcileDeleteControllerManager,
		r.reconcileDeleteCloudMachine,
		// Note: We are not deleting kubeadm objects because they exist in K8s, they are not related to a specific machine.
	}

	res := ctrl.Result{}
	errs := []error{}
	for _, phase := range phases {
		phaseResult, err := phase(ctx, cluster, machine, inMemoryMachine)
		if err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			continue
		}
		res = util.LowestNonZeroResult(res, phaseResult)
	}
	if res.IsZero() && len(errs) == 0 {
		controllerutil.RemoveFinalizer(inMemoryMachine, infrav1.MachineFinalizer)
	}
	return res, kerrors.NewAggregate(errs)
}

func (r *InMemoryMachineReconciler) reconcileDeleteCloudMachine(ctx context.Context, cluster *clusterv1.Cluster, _ *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Delete VM
	cloudMachine := &cloudv1.CloudMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: inMemoryMachine.Name,
		},
	}
	if err := cloudClient.Delete(ctx, cloudMachine); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete CloudMachine")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDeleteNode(ctx context.Context, cluster *clusterv1.Cluster, _ *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	// Delete Node
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: inMemoryMachine.Name,
		},
	}
	if err := cloudClient.Delete(ctx, node); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete Node")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDeleteETCD(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	etcdMember := fmt.Sprintf("etcd-%s", inMemoryMachine.Name)
	etcdPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      etcdMember,
		},
	}
	if err := cloudClient.Delete(ctx, etcdPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete etcd Pod")
	}
	if err := r.APIServerMux.DeleteEtcdMember(resourceGroup, etcdMember); err != nil {
		return ctrl.Result{}, err
	}

	// TODO: if all the etcd members are gone, cleanup all the k8s objects from the resource group.
	// note: it is not possible to delete the resource group, because cloud resources should be preserved.
	// given that, in order to implement this it is required to find a way to identify all the k8s resources (might be via gvk);
	// also, deletion must happen suddenly, without respecting finalizers or owner references links.

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDeleteAPIServer(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	apiServer := fmt.Sprintf("kube-apiserver-%s", inMemoryMachine.Name)
	apiServerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      apiServer,
		},
	}
	if err := cloudClient.Delete(ctx, apiServerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to delete apiServer Pod")
	}
	if err := r.APIServerMux.DeleteAPIServer(resourceGroup, apiServer); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDeleteScheduler(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	schedulerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-scheduler-%s", inMemoryMachine.Name),
		},
	}
	if err := cloudClient.Delete(ctx, schedulerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to scheduler Pod")
	}

	return ctrl.Result{}, nil
}

func (r *InMemoryMachineReconciler) reconcileDeleteControllerManager(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine, inMemoryMachine *infrav1.InMemoryMachine) (ctrl.Result, error) {
	// No-op if the machine is not a control plane machine.
	if !util.IsControlPlaneMachine(machine) {
		return ctrl.Result{}, nil
	}

	// Compute the resource group unique name.
	// NOTE: We are using reconcilerGroup also as a name for the listener for sake of simplicity.
	resourceGroup := klog.KObj(cluster).String()
	cloudClient := r.CloudManager.GetResourceGroup(resourceGroup).GetClient()

	controllerManagerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceSystem,
			Name:      fmt.Sprintf("kube-controller-manager-%s", inMemoryMachine.Name),
		},
	}
	if err := cloudClient.Delete(ctx, controllerManagerPod); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, errors.Wrapf(err, "failed to controller manager Pod")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager will add watches for this controller.
func (r *InMemoryMachineReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	clusterToInMemoryMachines, err := util.ClusterToObjectsMapper(mgr.GetClient(), &infrav1.InMemoryMachineList{}, mgr.GetScheme())
	if err != nil {
		return err
	}

	err = ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.InMemoryMachine{}).
		WithOptions(options).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(ctrl.LoggerFrom(ctx), r.WatchFilterValue)).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(util.MachineToInfrastructureMapFunc(infrav1.GroupVersion.WithKind("InMemoryMachine"))),
		).
		Watches(
			&infrav1.InMemoryCluster{},
			handler.EnqueueRequestsFromMapFunc(r.InMemoryClusterToInMemoryMachines),
		).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToInMemoryMachines),
			builder.WithPredicates(
				predicates.ClusterUnpausedAndInfrastructureReady(ctrl.LoggerFrom(ctx)),
			),
		).Complete(r)
	if err != nil {
		return errors.Wrap(err, "failed setting up with a controller manager")
	}
	return nil
}

// InMemoryClusterToInMemoryMachines is a handler.ToRequestsFunc to be used to enqueue
// requests for reconciliation of InMemoryMachines.
func (r *InMemoryMachineReconciler) InMemoryClusterToInMemoryMachines(ctx context.Context, o client.Object) []ctrl.Request {
	result := []ctrl.Request{}
	c, ok := o.(*infrav1.InMemoryCluster)
	if !ok {
		panic(fmt.Sprintf("Expected a InMemoryCluster but got a %T", o))
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, c.ObjectMeta)
	switch {
	case apierrors.IsNotFound(err) || cluster == nil:
		return result
	case err != nil:
		return result
	}

	labels := map[string]string{clusterv1.ClusterNameLabel: cluster.Name}
	machineList := &clusterv1.MachineList{}
	if err := r.Client.List(ctx, machineList, client.InNamespace(c.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil
	}
	for _, m := range machineList.Items {
		if m.Spec.InfrastructureRef.Name == "" {
			continue
		}
		name := client.ObjectKey{Namespace: m.Namespace, Name: m.Name}
		result = append(result, ctrl.Request{NamespacedName: name})
	}

	return result
}
