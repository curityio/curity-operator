package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"errors"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// IdentityServerNodeReconciler reconciles an IdentityServerNode object.
// It creates and manages a Deployment and Service for each node.
type IdentityServerNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=curity.io,resources=identityservernodes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=curity.io,resources=identityservernodes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=curity.io,resources=identityservernodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=curity.io,resources=identityserverclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles a single reconciliation loop for an IdentityServerNode.
func (r *IdentityServerNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	// 1. Fetch the IdentityServerNode
	var node v1alpha1.IdentityServerNode
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerNode: %w", err)
	}

	// 2. Handle finalizer
	if node.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(&node, v1alpha1.NodeFinalizer) {
			log.Info("deleting IdentityServerNode", "name", node.Name)
			r.Recorder.Event(&node, corev1.EventTypeNormal, "Deleting", "Node is being deleted")
			controllerutil.RemoveFinalizer(&node, v1alpha1.NodeFinalizer)
			if err := r.Update(ctx, &node); err != nil {
				if apierrors.IsConflict(err) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// 3. Ensure finalizer and cluster label are set (combined to reduce API round trips)
	clusterRef := node.Spec.IdentityServerClusterRef
	needsUpdate := false
	if !controllerutil.ContainsFinalizer(&node, v1alpha1.NodeFinalizer) {
		controllerutil.AddFinalizer(&node, v1alpha1.NodeFinalizer)
		needsUpdate = true
	}
	if node.Labels == nil || node.Labels["curity.io/cluster"] != clusterRef.Name {
		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}
		node.Labels["curity.io/cluster"] = clusterRef.Name
		needsUpdate = true
	}
	if needsUpdate {
		if err := r.Update(ctx, &node); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to update node metadata: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 4. Fetch the referenced IdentityServerCluster
	// (clusterRef was already extracted in step 3)
	clusterNS := clusterRef.Namespace
	if clusterNS == "" {
		clusterNS = node.Namespace
	}

	var cluster v1alpha1.IdentityServerCluster
	if err := r.Get(ctx, client.ObjectKey{Name: clusterRef.Name, Namespace: clusterNS}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("referenced cluster not found", "cluster", clusterRef.Name)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				"ClusterNotFound", fmt.Sprintf("Referenced cluster %q not found", clusterRef.Name), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				"ClusterNotFound", "Referenced cluster not found", node.Generation)
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "ClusterNotFound",
				"Referenced IdentityServerCluster %q not found in namespace %q", clusterRef.Name, clusterNS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerCluster: %w", err)
	}

	// 5. Validation: check for duplicate admin nodes and duplicate roles.
	// NOTE: This list-then-check approach has a known TOCTOU race — two admin
	// nodes created simultaneously could both pass validation before either's
	// Deployment exists. A validating admission webhook (Phase 2) will close
	// this gap with atomic server-side enforcement.
	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList,
		client.InNamespace(node.Namespace),
		client.MatchingLabels{"curity.io/cluster": clusterRef.Name},
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list nodes: %w", err)
	}
	for i := range nodeList.Items {
		other := &nodeList.Items[i]
		if other.Name == node.Name {
			continue
		}

		// Admin guard: only one admin per cluster
		if node.Spec.Type == v1alpha1.NodeTypeAdmin &&
			other.Spec.Type == v1alpha1.NodeTypeAdmin {
			log.Info("another admin node already exists", "existing", other.Name)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				"DuplicateAdmin", fmt.Sprintf("Another admin node %q already exists for cluster %q", other.Name, clusterRef.Name), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				"DuplicateAdmin", "Another admin node already exists for this cluster", node.Generation)
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "DuplicateAdmin",
				"Another admin node %q already exists for cluster %q", other.Name, clusterRef.Name)
			return ctrl.Result{}, nil
		}

		// Role uniqueness: each role must be unique per cluster
		if other.Spec.Role == node.Spec.Role {
			log.Info("duplicate role in cluster", "role", node.Spec.Role, "existing", other.Name)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				"DuplicateRole", fmt.Sprintf("Role %q is already used by node %q in cluster %q", node.Spec.Role, other.Name, clusterRef.Name), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				"DuplicateRole", fmt.Sprintf("Role %q must be unique per cluster", node.Spec.Role), node.Generation)
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "DuplicateRole",
				"Role %q is already used by node %q in cluster %q", node.Spec.Role, other.Name, clusterRef.Name)
			return ctrl.Result{}, nil
		}
	}

	// Default admin credentials when not specified (matches cluster reconciler behavior)
	if cluster.Spec.AdminCredentials == nil {
		cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
	}

	// 5.5. Discover managed ConfigMaps/Secrets and check validation status.
	// The cluster reconciler handles validation via a Job. The node reconciler
	// only mounts configs that the cluster has already validated.
	allConfigs, err := discoverConfigResources(ctx, r.Client, node.Namespace)
	if err != nil {
		if errors.Is(err, ErrUnknownConfigType) {
			// Config type error is also surfaced by the cluster reconciler via condition.
			// Record an event on the node so users see it on kubectl describe.
			log.Info("skipping config mounting due to unknown config type", "error", err.Error())
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "UnknownConfigType", "%s", err)
			allConfigs = nil
		} else {
			return ctrl.Result{}, fmt.Errorf("discovering managed configs: %w", err)
		}
	}

	// Determine admin routing.
	adminExists := findAdminNodeName(nodeList.Items) != ""
	var applicableConfigs []DiscoveredConfigResource
	if shouldMountConfig(node.Spec.Type, adminExists) {
		applicableConfigs = allConfigs
	}

	// Check if the cluster has validated the current config set.
	var validatedConfigs []DiscoveredConfigResource
	configHash := computeConfigHash(applicableConfigs)

	if len(applicableConfigs) > 0 {
		validatedHash := cluster.Annotations[annotationValidatedConfigHash]
		if validatedHash == configHash {
			validatedConfigs = applicableConfigs
			node.Status.AppliedConfigs = buildAppliedConfigStatus(applicableConfigs, v1alpha1.ValidationStatusValidated)
		} else {
			// Configs not yet validated — don't mount. The cluster reconciler
			// will create the validation Job and update the annotation.
			node.Status.AppliedConfigs = buildAppliedConfigStatus(applicableConfigs, v1alpha1.ValidationStatusPending)
		}
	} else {
		node.Status.AppliedConfigs = nil
	}

	// 5.6. Create Service before the Deployment gate below. For admin nodes, this
	// ensures the genclust Job can resolve the admin hostname via DNS even before
	// the Deployment exists.
	desiredSvc := buildService(&cluster, &node)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}

	svcResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desiredSvc.Labels
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return controllerutil.SetControllerReference(&node, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile Service: %w", err)
	}
	if svcResult != controllerutil.OperationResultNone {
		log.Info("Service reconciled", "operation", svcResult, "name", svc.Name)
		r.Recorder.Eventf(&node, corev1.EventTypeNormal, "ServiceReconciled",
			"Service %q %s", svc.Name, svcResult)
	}

	// 5.7. Gate: defer Deployment creation until cluster config is ready.
	// When an admin node exists, the cluster reconciler generates cluster.xml
	// via a genclust Job. Deferring prevents a double rollout caused by hashing
	// the placeholder first and the real data second.
	// Runtime-only clusters (no admin) skip — no genclust Job runs.
	if adminExists {
		configReady := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
		if configReady == nil || configReady.Status != metav1.ConditionTrue {
			var existingDeploy appsv1.Deployment
			err := r.Get(ctx, client.ObjectKey{Name: node.Name, Namespace: node.Namespace}, &existingDeploy)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("checking existing deployment for config gate: %w", err)
			}
			if err == nil {
				log.V(1).Info("config gate bypassed — Deployment already exists", "cluster", cluster.Name, "deployment", node.Name)
			}
			if apierrors.IsNotFound(err) {
				log.Info("waiting for ClusterConfigReady before creating Deployment", "cluster", cluster.Name)
				setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
					"WaitingForClusterConfig", "Waiting for cluster configuration to be generated", node.Generation)
				node.Status.ObservedGeneration = node.Generation
				node.Status.ServiceName = svc.Name
				if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
					if apierrors.IsConflict(statusErr) {
						return ctrl.Result{Requeue: true}, nil
					}
					return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
				}
				r.Recorder.Eventf(&node, corev1.EventTypeNormal, "WaitingForClusterConfig",
					"Deferring Deployment creation until cluster %q ClusterConfigReady=True", cluster.Name)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
		}
	}

	// 6. Build and reconcile the Deployment
	as := resolveAutoscaling(&cluster, &node)
	desiredDeploy := buildDeployment(&cluster, &node, validatedConfigs)

	// Inject cluster config hash annotation for rolling restart when Secret changes.
	// Only inject when config is ready or no admin exists — avoids hashing
	// placeholder data during config generation, which would cause a double rollout.
	configReadyCond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
	clusterConfigIsReady := !adminExists || (configReadyCond != nil && configReadyCond.Status == metav1.ConditionTrue)
	if clusterConfigIsReady {
		configSecretName := cluster.Name + "-cluster-config"
		var configSecret corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Name: configSecretName, Namespace: node.Namespace}, &configSecret); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("getting cluster-config secret %q: %w", configSecretName, err)
			}
		} else if data, ok := configSecret.Data["cluster.xml"]; ok && len(data) > 0 {
			h := sha256.Sum256(data)
			if desiredDeploy.Spec.Template.Annotations == nil {
				desiredDeploy.Spec.Template.Annotations = make(map[string]string)
			}
			desiredDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"] = hex.EncodeToString(h[:])
		}
	}

	// Inject discovered config hash annotation for rolling restart on config changes.
	if configHash != "" {
		if desiredDeploy.Spec.Template.Annotations == nil {
			desiredDeploy.Spec.Template.Annotations = make(map[string]string)
		}
		desiredDeploy.Spec.Template.Annotations[annotationConfigHash] = configHash
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		currentReplicas := deploy.Spec.Replicas
		existingConfigHash := deploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]

		deploy.Labels = desiredDeploy.Labels
		deploy.Spec = desiredDeploy.Spec

		// Preserve cluster-config-hash if the desired Deployment doesn't set one.
		// This avoids unnecessary rollouts during config regeneration (e.g.,
		// encryption key rotation) where the clusterConfigIsReady gate above
		// skips injection.
		if _, hasDesired := desiredDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"]; !hasDesired && existingConfigHash != "" {
			log.Info("preserving existing cluster-config-hash while config is regenerating",
				"hash", existingConfigHash, "cluster", cluster.Name)
			if deploy.Spec.Template.Annotations == nil {
				deploy.Spec.Template.Annotations = make(map[string]string)
			}
			deploy.Spec.Template.Annotations["curity.io/cluster-config-hash"] = existingConfigHash
		}

		// Preserve HPA-managed replicas on update to avoid replica flapping.
		if as != nil && as.Enabled &&
			node.Spec.Type != v1alpha1.NodeTypeAdmin &&
			currentReplicas != nil {
			deploy.Spec.Replicas = currentReplicas
		}
		return controllerutil.SetControllerReference(&node, deploy, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile Deployment: %w", err)
	}
	if result != controllerutil.OperationResultNone {
		log.Info("Deployment reconciled", "operation", result, "name", deploy.Name)
		r.Recorder.Eventf(&node, corev1.EventTypeNormal, "DeploymentReconciled",
			"Deployment %q %s", deploy.Name, result)
	}

	// 7. Reconcile HorizontalPodAutoscaler
	if node.Spec.Type != v1alpha1.NodeTypeAdmin && as != nil && as.Enabled {
		desiredHPA := buildHPA(&cluster, &node, as)
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      desiredHPA.Name,
				Namespace: desiredHPA.Namespace,
			},
		}
		result, err = controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
			hpa.Labels = desiredHPA.Labels
			hpa.Spec = desiredHPA.Spec
			return controllerutil.SetControllerReference(&node, hpa, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile HPA: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("HPA reconciled", "operation", result, "name", hpa.Name)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "HPAReconciled",
				"HorizontalPodAutoscaler %q %s", hpa.Name, result)
		}
	} else {
		// HPA not needed (admin node, autoscaling disabled, or nil) — clean up if stale.
		var existingHPA autoscalingv2.HorizontalPodAutoscaler
		if err := r.Get(ctx, client.ObjectKey{
			Name: node.Name, Namespace: node.Namespace,
		}, &existingHPA); err == nil {
			if !metav1.IsControlledBy(&existingHPA, &node) {
				log.Info("skipping HPA deletion, not owned by this node", "name", existingHPA.Name)
				r.Recorder.Eventf(&node, corev1.EventTypeWarning, "HPANotOwned",
					"HPA %q exists but is not managed by this node; skipping deletion", existingHPA.Name)
			} else {
				if err := r.Delete(ctx, &existingHPA); err != nil {
					if !apierrors.IsNotFound(err) {
						return ctrl.Result{}, fmt.Errorf("failed to delete HPA: %w", err)
					}
				} else {
					log.Info("HPA deleted", "name", existingHPA.Name)
					if node.Spec.Type == v1alpha1.NodeTypeAdmin {
						r.Recorder.Eventf(&node, corev1.EventTypeWarning, "AutoscalingIgnored",
							"HPA %q deleted: autoscaling is not supported on admin nodes", existingHPA.Name)
					} else {
						r.Recorder.Eventf(&node, corev1.EventTypeNormal, "HPADeleted",
							"HorizontalPodAutoscaler %q deleted", existingHPA.Name)
					}
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get HPA: %w", err)
		}
	}

	// 9. Compute status from Deployment
	conditions, rs := computeNodeConditions(deploy, node.Generation)
	node.Status.Conditions = conditions
	node.Status.ObservedGeneration = node.Generation
	node.Status.Replicas = rs.Replicas
	node.Status.UpdatedReplicas = rs.UpdatedReplicas
	node.Status.ReadyReplicas = rs.ReadyReplicas
	node.Status.AvailableReplicas = rs.AvailableReplicas
	node.Status.UnavailableReplicas = rs.UnavailableReplicas
	node.Status.DeploymentName = deploy.Name
	node.Status.ServiceName = svc.Name

	// 10. Update status
	if err := r.Status().Update(ctx, &node); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to update node status: %w", err)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager.
func (r *IdentityServerNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.IdentityServerNode{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForCluster),
			builder.WithPredicates(clusterSpecOrNodeCountChangedPredicate{}),
		).
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Complete(r)
}

// findNodesForCluster maps a cluster change to reconcile requests for all
// nodes that reference it, so changes to shared config (logging, image, etc.)
// propagate to node Deployments.
func (r *IdentityServerNodeReconciler) findNodesForCluster(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*v1alpha1.IdentityServerCluster)
	if !ok {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)

	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"curity.io/cluster": cluster.Name},
	); err != nil {
		log.Error(err, "failed to list nodes for cluster watch", "cluster", cluster.Name)
		return nil
	}

	var requests []ctrl.Request
	for i := range nodeList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&nodeList.Items[i]),
		})
	}
	return requests
}

// findNodesForManagedConfig maps a managed ConfigMap/Secret change to reconcile
// requests for all IdentityServerNodes in the same namespace.
func (r *IdentityServerNodeReconciler) findNodesForManagedConfig(ctx context.Context, obj client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList, client.InNamespace(obj.GetNamespace())); err != nil {
		log.Error(err, "failed to list nodes for managed config watch", "resource", obj.GetName())
		return nil
	}

	requests := make([]ctrl.Request, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&nodeList.Items[i]),
		})
	}
	return requests
}
