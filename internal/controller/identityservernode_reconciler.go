package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// annotationManagedConfigsHash is stamped on the Deployment pod template and
// changes when the discovered managed-config set changes, forcing a rolling
// restart so idsvr re-reads its config files. SubPath mounts do not
// auto-refresh, so without this, edits to managed CMs/Secrets would never
// reach the running idsvr process.
const annotationManagedConfigsHash = "curity.io/managed-configs-hash"

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
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

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
			setCondition(&node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionFalse,
				v1alpha1.ReasonClusterNotFound, fmt.Sprintf("Referenced cluster %q not found", clusterRef.Name), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				v1alpha1.ReasonClusterNotFound, fmt.Sprintf("Referenced cluster %q not found", clusterRef.Name), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				v1alpha1.ReasonClusterNotFound, "Referenced cluster not found", node.Generation)
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				if apierrors.IsConflict(statusErr) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, v1alpha1.ReasonClusterNotFound,
				"Referenced IdentityServerCluster %q not found in namespace %q", clusterRef.Name, clusterNS)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerCluster: %w", err)
	}

	// Ensure controller OwnerReference from cluster → node (idempotent).
	// Provides cascade-delete safety under Foreground propagation; the
	// cluster-side finalizer is what actually blocks cluster deletion
	// until all child nodes are gone.
	if changed := ensureClusterOwnerRef(&node, &cluster); changed {
		if err := r.Update(ctx, &node); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to set OwnerReference on node: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	setCondition(&node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionTrue,
		v1alpha1.ReasonClusterFound, fmt.Sprintf("Referenced cluster %q found", cluster.Name), node.Generation)

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
				if apierrors.IsConflict(statusErr) {
					return ctrl.Result{Requeue: true}, nil
				}
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
				if apierrors.IsConflict(statusErr) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "DuplicateRole",
				"Role %q is already used by node %q in cluster %q", node.Spec.Role, other.Name, clusterRef.Name)
			return ctrl.Result{}, nil
		}
	}

	// 5.1. Cross-cluster ownedResourceName collision check.
	// ownedResourceName joins clusterName and nodeName with "-", which is
	// ambiguous: ("foo-bar","baz") and ("foo","bar-baz") both yield
	// "foo-bar-baz". Without this guard, two nodes would fight over one
	// Deployment/Service/HPA/PDB via CreateOrUpdate. Tie-break: the older
	// node wins (CreationTimestamp, UID); the newer goes Degraded.
	// Shares the same TOCTOU caveat as the duplicate-admin/role checks above.
	candidateOwned := ownedResourceName(clusterRef.Name, node.Name)
	// Capture whether the previous reconcile saw this node as colliding, so
	// we can emit a ResolvedOwnedNameCollision Normal event once we fall
	// through the check cleanly. Without the matched pair, operators see
	// "it broke" via Warning but never "it unbroke".
	priorDegraded := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionDegraded)
	priorWasCollision := priorDegraded != nil &&
		priorDegraded.Status == metav1.ConditionTrue &&
		priorDegraded.Reason == v1alpha1.ReasonOwnedNameCollision
	var allNodes v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &allNodes, client.InNamespace(node.Namespace)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to list nodes for name-collision check: %w", err)
	}
	for i := range allNodes.Items {
		other := &allNodes.Items[i]
		if other.UID == node.UID {
			continue
		}
		if ownedResourceName(other.Spec.IdentityServerClusterRef.Name, other.Name) != candidateOwned {
			continue
		}
		if !nodeIsNewer(&node, other) {
			continue
		}
		msg := fmt.Sprintf(
			"Child resource name %q collides with IdentityServerNode %q in cluster %q",
			candidateOwned, other.Name, other.Spec.IdentityServerClusterRef.Name)
		log.Info("owned resource name collision", "name", candidateOwned, "collidesWith", other.Name)
		setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
			v1alpha1.ReasonOwnedNameCollision, msg, node.Generation)
		setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
			v1alpha1.ReasonOwnedNameCollision, msg, node.Generation)
		node.Status.ObservedGeneration = node.Generation
		if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
			if apierrors.IsConflict(statusErr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
		}
		r.Recorder.Eventf(&node, corev1.EventTypeWarning, v1alpha1.ReasonOwnedNameCollision, "%s", msg)
		return ctrl.Result{}, nil
	}
	if priorWasCollision {
		r.Recorder.Eventf(&node, corev1.EventTypeNormal, "ResolvedOwnedNameCollision",
			"ownedResourceName %q no longer collides with any peer; previous conflict resolved",
			candidateOwned)
	}

	// Default admin credentials when not specified (matches cluster reconciler behavior)
	if cluster.Spec.AdminCredentials == nil {
		cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
	}

	// 5.5. Discover managed ConfigMaps/Secrets and check validation status.
	// The cluster reconciler handles validation via a Job. The node reconciler
	// only mounts configs that the cluster has already validated.
	// Resources with invalid curity.io/config-type annotations come back as
	// `skipped` instead of failing the whole call. Emit one Warning per bad
	// resource on the node (so `kubectl describe node` shows it) and then
	// mount the valid subset.
	allConfigs, skipped, err := discoverConfigResources(ctx, r.Client, node.Namespace, clusterRef.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("discovering managed configs: %w", err)
	}
	for _, sk := range skipped {
		log.Info("skipping managed resource with invalid config-type annotation",
			"kind", sk.Kind, "name", sk.Name, "reason", sk.Reason)
		r.Recorder.Eventf(&node, corev1.EventTypeWarning, "UnknownConfigType",
			"Skipped %s/%s: %s", sk.Kind, sk.Name, sk.Reason)
	}

	// Determine admin routing.
	adminExists := findAdminNodeName(nodeList.Items) != ""
	var applicableConfigs []DiscoveredConfigResource
	if shouldMountConfig(node.Spec.Type, adminExists) {
		applicableConfigs = allConfigs
	}

	// Hash drives the pod-template config-hash annotation, which triggers
	// rolling restart on config-content change.
	configHash := computeConfigHash(applicableConfigs)

	if len(applicableConfigs) > 0 {
		node.Status.AppliedConfigs = buildAppliedConfigStatus(applicableConfigs)
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
			deployName := ownedResourceName(cluster.Name, node.Name)
			err := r.Get(ctx, client.ObjectKey{Name: deployName, Namespace: node.Namespace}, &existingDeploy)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("checking existing deployment for config gate: %w", err)
			}
			if err == nil {
				log.V(1).Info("config gate bypassed — Deployment already exists", "cluster", cluster.Name, "deployment", deployName)
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
	desiredDeploy := buildDeployment(&cluster, &node, applicableConfigs)

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

	// Inject managed-configs-hash annotation for rolling restart on config changes.
	if configHash != "" {
		if desiredDeploy.Spec.Template.Annotations == nil {
			desiredDeploy.Spec.Template.Annotations = make(map[string]string)
		}
		desiredDeploy.Spec.Template.Annotations[annotationManagedConfigsHash] = configHash
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
			Name: ownedResourceName(cluster.Name, node.Name), Namespace: node.Namespace,
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

	// 8. Reconcile PodDisruptionBudget (runtime nodes only, when minAvailable is set).
	pdbSpec := resolvePDB(&cluster, &node)
	pdbRequested := pdbSpec != nil && pdbSpec.MinAvailable != nil
	isAdmin := node.Spec.Type == v1alpha1.NodeTypeAdmin

	// Admin guard: log + event whenever PDB is requested on an admin node, regardless
	// of whether a stale PDB exists. K8s Event server-side dedup absorbs repeats.
	if isAdmin && pdbRequested {
		log.Info("ignoring PDB spec on admin node", "node", node.Name, "minAvailable", pdbSpec.MinAvailable)
		r.Recorder.Eventf(&node, corev1.EventTypeWarning, "PDBIgnored",
			"PodDisruptionBudget spec is set but ignored: PDB is not supported on admin nodes")
	}

	// pdbCollisionName is non-empty when cleanup found a PDB with our target
	// name that is not owned by this node. We record it here and apply the
	// Degraded condition *after* computeNodeConditions, which rebuilds the
	// conditions slice from scratch.
	var pdbCollisionName string

	if !isAdmin && pdbRequested {
		desiredPDB := buildPDB(&cluster, &node)
		existingPDB := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: desiredPDB.Name, Namespace: desiredPDB.Namespace},
		}
		result, err = controllerutil.CreateOrUpdate(ctx, r.Client, existingPDB, func() error {
			existingPDB.Labels = desiredPDB.Labels
			existingPDB.Spec = desiredPDB.Spec
			return controllerutil.SetControllerReference(&node, existingPDB, r.Scheme)
		})
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to reconcile PDB: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("PDB reconciled", "operation", result, "name", existingPDB.Name)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PDBReconciled",
				"PodDisruptionBudget %q %s", existingPDB.Name, result)
		}

		// Post-write sanity check: if the PDB controller has marked the budget
		// unsatisfiable (no disruptions allowed AND the desired health is unmet),
		// emit a Warning so `kubectl drain` doesn't silently hang for the user.
		//
		// Gate on ObservedGeneration == Generation. The PDB controller in
		// kube-controller-manager populates .status asynchronously, so on Create
		// the status is zero (leading to a false-negative skip), and on spec
		// updates DesiredHealthy can briefly reflect the previous MinAvailable
		// (leading to a false-positive warning right after the user edits a
		// misconfig back into a valid state). When status is stale relative to
		// spec we defer; the Owns(&PodDisruptionBudget{}) watch will re-enqueue
		// once the PDB controller catches up.
		if existingPDB.Status.ObservedGeneration == existingPDB.Generation &&
			existingPDB.Status.DisruptionsAllowed == 0 &&
			existingPDB.Status.DesiredHealthy > existingPDB.Status.CurrentHealthy {
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "PDBUnsatisfiable",
				"PodDisruptionBudget %q cannot be satisfied: desiredHealthy=%d, currentHealthy=%d; voluntary disruptions are blocked",
				existingPDB.Name, existingPDB.Status.DesiredHealthy, existingPDB.Status.CurrentHealthy)
		}
	} else {
		// Delete a stale operator-owned PDB if one exists; refuse to delete one we
		// don't own (surface it via a Degraded condition so users see the collision).
		var existingPDB policyv1.PodDisruptionBudget
		err := r.Get(ctx, client.ObjectKey{Name: ownedResourceName(cluster.Name, node.Name), Namespace: node.Namespace}, &existingPDB)
		if err == nil {
			if !metav1.IsControlledBy(&existingPDB, &node) {
				log.Info("skipping PDB deletion, not owned by this node", "name", existingPDB.Name)
				r.Recorder.Eventf(&node, corev1.EventTypeWarning, "PDBNotOwned",
					"PDB %q exists but is not managed by this node; skipping deletion", existingPDB.Name)
				pdbCollisionName = existingPDB.Name
			} else if delErr := r.Delete(ctx, &existingPDB); delErr != nil && !apierrors.IsNotFound(delErr) {
				return ctrl.Result{}, fmt.Errorf("failed to delete PDB: %w", delErr)
			} else {
				log.Info("PDB deleted", "name", existingPDB.Name)
				// Admin-case warning was already emitted above; only emit the
				// Normal cleanup event for the runtime "spec removed" case.
				if !isAdmin {
					r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PDBDeleted",
						"PodDisruptionBudget %q deleted", existingPDB.Name)
				}
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get PDB: %w", err)
		}
	}

	// 9. Compute status from Deployment
	conditions, rs := computeNodeConditions(deploy, node.Generation)
	node.Status.Conditions = conditions

	// Re-apply ClusterReady — computeNodeConditions rebuilds the slice from
	// Deployment health and would otherwise drop it.
	setCondition(&node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionTrue,
		v1alpha1.ReasonClusterFound, fmt.Sprintf("Referenced cluster %q found", cluster.Name), node.Generation)

	// Overlay PDB-specific Degraded condition after computeNodeConditions, which
	// rebuilds conditions from Deployment health. A PDB name collision is
	// higher-priority than partial-replica degradation — if both apply, the PDB
	// message is more actionable.
	if pdbCollisionName != "" {
		setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"PDBNotOwned",
			fmt.Sprintf("A PodDisruptionBudget named %q exists but is not owned by this node", pdbCollisionName),
			node.Generation)
	}

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
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForCluster),
			builder.WithPredicates(clusterSpecOrNodeCountChangedPredicate{}),
		).
		Watches(
			&corev1.ConfigMap{},
			newManagedConfigHandler(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			newManagedConfigHandler(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Complete(r)
}

// nodeIsNewer reports whether a sorts after b under (CreationTimestamp, UID)
// ASC ordering. Both sides of a collision compute the same result, so the
// Degraded verdict is symmetric: only one of the two nodes marks itself the
// loser. UID is the tie-break because CreationTimestamp has 1-second
// resolution and two CRs created in the same reconcile tick can share it.
func nodeIsNewer(a, b *v1alpha1.IdentityServerNode) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.After(b.CreationTimestamp.Time)
	}
	return string(a.UID) > string(b.UID)
}

// ensureClusterOwnerRef returns true if it mutated node.OwnerReferences.
// Walks the full slice instead of early-returning on first match so stale
// duplicates (different UID, missing Controller/BlockOwnerDeletion flags,
// leftover refs to a deleted cluster) get dropped — exactly one canonical
// ref of our APIVersion+Kind remains on return.
func ensureClusterOwnerRef(node *v1alpha1.IdentityServerNode, cluster *v1alpha1.IdentityServerCluster) bool {
	want := metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "IdentityServerCluster",
		Name:               cluster.Name,
		UID:                cluster.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
	filtered := make([]metav1.OwnerReference, 0, len(node.OwnerReferences)+1)
	kept := false
	for _, ref := range node.OwnerReferences {
		if ref.APIVersion == want.APIVersion && ref.Kind == want.Kind {
			if !kept && ownerRefMatches(ref, want) {
				filtered = append(filtered, ref)
				kept = true
			}
			continue
		}
		filtered = append(filtered, ref)
	}
	if !kept {
		filtered = append(filtered, want)
	}
	if ownerRefsEqual(node.OwnerReferences, filtered) {
		return false
	}
	node.OwnerReferences = filtered
	return true
}

// ownerRefMatches treats nil Controller / BlockOwnerDeletion pointers as
// not-a-match: both flags must be true for the finalizer-based deletion
// ordering to hold.
func ownerRefMatches(got, want metav1.OwnerReference) bool {
	return got.UID == want.UID &&
		got.Name == want.Name &&
		got.Controller != nil && *got.Controller &&
		got.BlockOwnerDeletion != nil && *got.BlockOwnerDeletion
}

func ownerRefsEqual(a, b []metav1.OwnerReference) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].APIVersion != b[i].APIVersion ||
			a[i].Kind != b[i].Kind ||
			a[i].Name != b[i].Name ||
			a[i].UID != b[i].UID ||
			!boolPtrEqual(a[i].Controller, b[i].Controller) ||
			!boolPtrEqual(a[i].BlockOwnerDeletion, b[i].BlockOwnerDeletion) {
			return false
		}
	}
	return true
}

func boolPtrEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
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
// requests for IdentityServerNodes whose referenced cluster is in scope per the
// object's curity.io/cluster annotation.
func (r *IdentityServerNodeReconciler) findNodesForManagedConfig(ctx context.Context, obj client.Object) []ctrl.Request {
	log := ctrl.LoggerFrom(ctx)

	// Defensive label check. managedConfigPredicate passes Updates through
	// when EITHER the old or new object is labeled (so label removal still
	// fires, un-mounting dropped configs), and managedConfigHandler then
	// invokes this mapper once per side. The object we receive here may
	// therefore not itself be labeled — e.g. the new side of a label-removal
	// Update. Without this check, an unlabeled object (whose scope annotation
	// is also typically missing) would fan out to every node in the namespace.
	if !hasManagedLabel(obj) {
		return nil
	}

	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList, client.InNamespace(obj.GetNamespace())); err != nil {
		log.Error(err, "failed to list nodes for managed config watch", "resource", obj.GetName())
		return nil
	}

	requests := make([]ctrl.Request, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if !appliesToCluster(obj.GetAnnotations(), n.Spec.IdentityServerClusterRef.Name) {
			continue
		}
		requests = append(requests, ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(n),
		})
	}
	return requests
}
