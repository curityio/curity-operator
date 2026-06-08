package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// annotationManagedConfigsHash is stamped on the Deployment pod template and
// changes when the discovered managed-config set changes, forcing a rolling
// restart so idsvr re-reads its config files. SubPath mounts do not
// auto-refresh, so without this, edits to managed CMs/Secrets would never
// reach the running idsvr process.
const annotationManagedConfigsHash = "curity.io/managed-configs-hash"

// HPAHealthGracePeriod gates Gate B for fresh HPAs. Variable (not const) so
// tests can shorten it; production callers must not mutate at runtime.
var HPAHealthGracePeriod = time.Minute

// IdentityServerNodeReconciler reconciles an IdentityServerNode object.
// It creates and manages a Deployment and Service for each node.
type IdentityServerNodeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// PackageFetcherImage is the init-container image used for all
	// spec.packages downloads. Resolved once via
	// ResolvePackageFetcherImage at controller construction (see
	// cmd/manager/main.go) so reconciles never re-read process env.
	// Empty value falls back to DefaultPackageFetcherImage at use site.
	PackageFetcherImage string

	// hpaUnsatisfiableSeen gates Gate B emission to per-condition transitions.
	// Zero value is valid (lazy init).
	hpaUnsatisfiableSeen hpaUnsatisfiableCache
}

// Node UID (not name) so a delete+recreate of the same name does not
// inherit stale transition stamps.
type hpaCondKey struct {
	NodeUID  types.UID
	CondType autoscalingv2.HorizontalPodAutoscalerConditionType
}

type hpaUnsatisfiableCache struct {
	mu      sync.Mutex
	entries map[hpaCondKey]time.Time
}

// transition returns true when t is strictly newer than the stored stamp
// for key (or no stamp exists), and updates the stamp as a side effect.
func (c *hpaUnsatisfiableCache) transition(key hpaCondKey, t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[hpaCondKey]time.Time)
	}
	prev, ok := c.entries[key]
	if ok && !t.After(prev) {
		return false
	}
	c.entries[key] = t
	return true
}

// reap drops entries for nodeUID whose CondType is no longer present-and-False
// in current — clears the cache on recovery so the next failure re-emits.
func (c *hpaUnsatisfiableCache) reap(nodeUID types.UID, current []autoscalingv2.HorizontalPodAutoscalerCondition) {
	failing := make(map[autoscalingv2.HorizontalPodAutoscalerConditionType]struct{}, len(current))
	for _, cond := range current {
		if cond.Status != corev1.ConditionFalse {
			continue
		}
		if cond.Type == autoscalingv2.AbleToScale || cond.Type == autoscalingv2.ScalingActive {
			failing[cond.Type] = struct{}{}
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.NodeUID != nodeUID {
			continue
		}
		if _, stillFailing := failing[key.CondType]; !stillFailing {
			delete(c.entries, key)
		}
	}
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
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

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
			original := node.DeepCopy()
			controllerutil.RemoveFinalizer(&node, v1alpha1.NodeFinalizer)
			if err := r.Patch(ctx, &node, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
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
	original := node.DeepCopy()
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
		// Patch, not Update: a full Update round-trips spec and omitempty would drop
		// an explicit empty list (e.g. initContainers: []). Lock guards finalizers.
		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("failed to update node metadata: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// 3.5. Cleanup orphan children from a prior clusterRef. Runs before the
	// destination cluster fetch and any validation so orphans are removed in
	// every code path: successful move, destination missing, duplicate-admin
	// reject, owned-name collision. Idempotent (no-op when no orphans exist).
	if err := r.cleanupOrphanChildren(ctx, &node); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleaning up orphan children: %w", err)
	}

	// 4. Fetch the referenced IdentityServerCluster
	// (clusterRef was already extracted in step 3)
	var cluster v1alpha1.IdentityServerCluster
	if err := r.Get(ctx, client.ObjectKey{Name: clusterRef.Name, Namespace: node.Namespace}, &cluster); err != nil {
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
				"Referenced IdentityServerCluster %q not found in namespace %q", clusterRef.Name, node.Namespace)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return ctrl.Result{}, fmt.Errorf("failed to get IdentityServerCluster: %w", err)
	}

	// Ensure controller OwnerReference from cluster → node (idempotent).
	// Provides cascade-delete safety under Foreground propagation; the
	// cluster-side finalizer is what actually blocks cluster deletion
	// until all child nodes are gone.
	ownerRefOriginal := node.DeepCopy()
	if changed := ensureClusterOwnerRef(&node, &cluster); changed {
		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(ownerRefOriginal, client.MergeFromWithOptimisticLock{})); err != nil {
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
			// Self-poll so a stuck node recovers without watching siblings:
			// a peer's spec mutation that resolves the conflict does not
			// change cluster.Status.NodeCount and so does not trigger our
			// watch. The 30s tick re-runs the validation; idempotent if the
			// conflict persists.
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
			// Self-poll so a stuck node recovers without watching siblings.
			// See DuplicateAdmin branch above for the full rationale.
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	// Default admin credentials when not specified (matches cluster reconciler behavior)
	if cluster.Spec.AdminCredentials == nil {
		cluster.Spec.AdminCredentials = defaultAdminCredentials(cluster.Name)
	}

	// 5.5. Discover managed ConfigMaps/Secrets and mount the valid subset.
	// Resources with invalid curity.io/config-type annotations are returned
	// in `skipped`; the cluster reconciler emits the user-visible event on
	// the offending CM/Secret (UID dedup would not collapse a per-node event).
	allConfigs, skipped, err := discoverManagedResources(ctx, r.Client, node.Namespace, clusterRef.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("discovering managed configs: %w", err)
	}
	for _, sk := range skipped {
		log.Info("skipping managed resource with invalid config-type annotation",
			"kind", sk.Kind, "name", sk.Name, "reason", sk.Reason)
	}

	// Issues are surfaced by the cluster reconciler; here we only use the
	// filtered slice so invalid/duplicate logging resources do not mount.
	allConfigs, loggingIssues := applyLoggingValidations(allConfigs)
	for _, li := range loggingIssues {
		log.Info("excluding logging resource from mount",
			"kind", li.Kind, "name", li.Name, "reason", li.EventReason)
	}

	adminExists := findAdminNodeName(nodeList.Items) != ""
	applicableConfigs := make([]DiscoveredManagedResource, 0, len(allConfigs))
	for _, cfg := range allConfigs {
		if shouldMountConfig(node.Spec.Type, adminExists, cfg.ConfigType) {
			applicableConfigs = append(applicableConfigs, cfg)
		}
	}

	// Hash drives the pod-template config-hash annotation, which triggers
	// rolling restart on config-content change.
	configHash := computeConfigHash(applicableConfigs)

	if len(applicableConfigs) > 0 {
		node.Status.AppliedManagedResources = buildAppliedManagedResources(applicableConfigs)
	} else {
		node.Status.AppliedManagedResources = nil
	}

	// 5.6. Create Service before the Deployment gate below. For admin nodes, this
	// ensures the genclust Job can resolve the admin hostname via DNS even before
	// the Deployment exists.
	desiredSvc := buildService(&cluster, &node)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}

	svcResult, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = mergeManagedLabels(svc.Labels, desiredSvc.Labels)
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return controllerutil.SetControllerReference(&node, svc, r.Scheme)
	})
	if err != nil {
		if res, handled, statusErr := r.handlePermanentWriteError(ctx, &node, err); handled {
			return res, statusErr
		}
		return ctrl.Result{}, fmt.Errorf("failed to reconcile Service: %w", err)
	}
	if svcResult != controllerutil.OperationResultNone {
		log.Info("Service reconciled", "operation", svcResult, "name", svc.Name)
		r.Recorder.Eventf(&node, corev1.EventTypeNormal, "ServiceReconciled",
			"Service %q %s", svc.Name, svcResult)
	}

	// 5.7. Defer Deployment writes while cluster.xml regenerates: the Secret
	// holds a placeholder during that window, and subPath mounts crashloop on
	// it. First-time create still returns early (no Deployment to keep alive).
	deferDeploymentUpdate := false
	var existingDeployForDefer *appsv1.Deployment
	if adminExists {
		configReady := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
		if configReady == nil || configReady.Status != metav1.ConditionTrue {
			var existingDeploy appsv1.Deployment
			deployName := OwnedResourceName(cluster.Name, node.Name)
			err := r.Get(ctx, client.ObjectKey{Name: deployName, Namespace: node.Namespace}, &existingDeploy)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("checking existing deployment for config gate: %w", err)
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
			deferDeploymentUpdate = true
			existingDeployForDefer = existingDeploy.DeepCopy()
			log.V(1).Info("deferring Deployment update until ClusterConfigReady=True", "cluster", cluster.Name, "deployment", deployName)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "DeferringDeploymentUpdate",
				"Deferring Deployment update until cluster %q ClusterConfigReady=True", cluster.Name)
		}
	}

	// 6. Build and reconcile the Deployment.
	// as/packagesHash/priorPackagesReadyForGate are read by HPA/status below
	// — hoisted out so they're set even when the defer branch skips the build.
	as := resolveAutoscaling(&cluster, &node)
	packagesHash := computePackagesHash(cluster.Spec.Packages)
	var priorPackagesReadyForGate *metav1.Condition
	if c := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady); c != nil {
		copied := *c
		priorPackagesReadyForGate = &copied
	}

	var deploy *appsv1.Deployment
	var result controllerutil.OperationResult
	if deferDeploymentUpdate {
		// Reuse the gate's Get to avoid a TOCTOU on re-fetch.
		deploy = existingDeployForDefer
	} else {
		fetcherImage := r.PackageFetcherImage
		if fetcherImage == "" {
			fetcherImage = DefaultPackageFetcherImage
		}

		// Warn once per spec edit when user podLabels collide with
		// operator-owned keys; runtime drops the user values silently.
		if overridden := userPodLabelsOverridden(&cluster, &node); len(overridden) > 0 &&
			node.Status.ObservedGeneration < node.Generation {
			log.Info("ignoring operator-owned keys in user podLabels",
				"node", node.Name, "keys", overridden)
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "PodLabelsIgnored",
				"podLabels keys %v are operator-owned; pods use operator values",
				overridden)
		}

		desiredDeploy := buildDeployment(&cluster, &node, applicableConfigs, fetcherImage)

		// Pre-flight: a user container whose name collides with an operator-
		// generated one (log sidecar, package fetcher, the main container) would
		// be rejected by the apiserver with a bare "Duplicate value". Surface the
		// source here so the message is self-explanatory, and skip the doomed write.
		if msg, conflict := detectContainerNameConflict(desiredDeploy, &cluster, &node); conflict {
			degChanged := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionDegraded,
				Status:             metav1.ConditionTrue,
				Reason:             v1alpha1.ReasonInvalidSpec,
				Message:            msg,
				ObservedGeneration: node.Generation,
			})
			apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionReady,
				Status:             metav1.ConditionFalse,
				Reason:             v1alpha1.ReasonInvalidSpec,
				Message:            "Deployment is invalid; see Degraded condition",
				ObservedGeneration: node.Generation,
			})
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				if apierrors.IsConflict(statusErr) {
					return ctrl.Result{Requeue: true}, nil
				}
				return ctrl.Result{}, fmt.Errorf("updating status after container-name conflict: %w", statusErr)
			}
			if degChanged {
				r.Recorder.Eventf(&node, corev1.EventTypeWarning, v1alpha1.ReasonInvalidSpec, "%s", msg)
			}
			return ctrl.Result{}, nil
		}

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

		// Inject packages-hash annotation for rolling restart on packages spec
		// changes. Hashes spec refs (Secret name/key, URL, mountPath, TLS shape)
		// — NOT Secret values — so token rotation in place does not roll the
		// deployment. See packages.go:computePackagesHash.
		if packagesHash != "" {
			if desiredDeploy.Spec.Template.Annotations == nil {
				desiredDeploy.Spec.Template.Annotations = make(map[string]string)
			}
			desiredDeploy.Spec.Template.Annotations[annotationPackagesHash] = packagesHash
		}

		deploy = &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}}

		// Capture the prev packages-hash with a dedicated Get before CreateOrUpdate
		// rather than from inside the mutator closure. Both work in principle, but
		// the dedicated Get is easier to reason about and decouples this from any
		// future change to controllerutil internals.
		var prevPackagesHash string
		existingDeploy := &appsv1.Deployment{}
		if err := r.Get(ctx, client.ObjectKey{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}, existingDeploy); err == nil {
			prevPackagesHash = existingDeploy.Spec.Template.Annotations[annotationPackagesHash]
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting existing deployment for prev-hash capture: %w", err)
		}

		// priorPackagesReadyForGate is captured above the defer branch so it is
		// available to the recovery check in step 9 even when this block is
		// skipped. It snapshots the condition before the pre-check below mutates
		// it in place — necessary because apimeta.FindStatusCondition returns a
		// slice-element pointer and setCondition rewrites that element to
		// PackagesPending, which would otherwise silently flip the captured
		// "prior" reason and break the recovery gate.

		// Package-secret pre-check (plan D1 + Secret-watch recovery). Two gates:
		//
		//  (1) Spec just changed — original gate; avoids stamping a broken spec
		//      onto the Deployment.
		//  (2) PackagesReady is currently in a recoverable failure — recovery
		//      probe woken by a Secret event. Without this, a user fixing a
		//      Secret has to wait for the 30 s requeue before recovery starts.
		//
		// We deliberately do NOT re-run pre-check in steady state on every
		// Secret event: once a package is downloaded into the pod's emptyDir,
		// the Secret is irrelevant to the running container. Flipping the
		// condition on Secret tampering while pods serve correctly is alarmism.
		if len(cluster.Spec.Packages) > 0 &&
			(prevPackagesHash != packagesHash || isRecoverableSecretFailure(priorPackagesReadyForGate)) {
			reason, message, retryErr := checkPackageSecrets(ctx, r.Client, cluster.Spec.Packages, node.Namespace)
			if retryErr != nil {
				// Non-IsNotFound API error — pre-check is inconclusive, do
				// not flip the condition. controller-runtime backs off on
				// the returned error (plan D13).
				return ctrl.Result{}, fmt.Errorf("package pre-check: %w", retryErr)
			}
			if reason != "" {
				setCondition(&node.Status.Conditions, v1alpha1.ConditionPackagesReady,
					metav1.ConditionFalse, reason, message, node.Generation)
				applyReadyOverlay(&node.Status.Conditions, node.Generation)
				if err := r.Status().Update(ctx, &node); err != nil {
					if apierrors.IsConflict(err) {
						return ctrl.Result{Requeue: true}, nil
					}
					return ctrl.Result{}, fmt.Errorf("failed to update node status after package pre-check: %w", err)
				}
				// Skip Deployment update — pushing a known-bad spec onto a
				// (possibly healthy) existing Deployment would crashloop
				// pods. The user fixes the spec or applies the missing
				// Secret; the 30s requeue retries when neither event woke
				// the reconciler in the meantime.
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// Pre-check passed AND the packages spec just changed (or no
			// Deployment exists yet). Preemptively set PackagesReady=False
			// reason=PackagesPending so the Ready overlay flips False on
			// this same reconcile — otherwise the still-healthy OLD pods
			// would leak PackagesReady=True (from the prior steady state)
			// through into the new rollout, producing a Ready=True
			// false-positive window until the new pod's init container
			// reports its first status (~8–15s on real clusters).
			// The translator below filters pods by curity.io/packages-hash
			// so it does not overwrite this Pending state with the OLD
			// pods' success; it only flips Pending → True or a specific
			// failure reason when a pod carrying the NEW hash reports
			// definitive init status.
			setCondition(&node.Status.Conditions, v1alpha1.ConditionPackagesReady,
				metav1.ConditionFalse, v1alpha1.ReasonPackagesPending,
				"package set rolling out; awaiting init-container state from pods carrying the new packages hash",
				node.Generation)
		}

		var err error
		result, err = controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
			currentReplicas := deploy.Spec.Replicas

			deploy.Labels = mergeManagedLabels(deploy.Labels, desiredDeploy.Labels)
			deploy.Spec = desiredDeploy.Spec

			// Preserve HPA-managed replicas on update to avoid replica flapping.
			if as != nil && as.Enabled &&
				node.Spec.Type != v1alpha1.NodeTypeAdmin &&
				currentReplicas != nil {
				deploy.Spec.Replicas = currentReplicas
			}
			return controllerutil.SetControllerReference(&node, deploy, r.Scheme)
		})
		if err != nil {
			if res, handled, statusErr := r.handlePermanentWriteError(ctx, &node, err); handled {
				return res, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("failed to reconcile Deployment: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("Deployment reconciled", "operation", result, "name", deploy.Name)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "DeploymentReconciled",
				"Deployment %q %s", deploy.Name, result)

			// Packages-specific events. Distinguish Configured (none -> some),
			// Updated (some -> different some), Removed (some -> none).
			// prevPackagesHash is captured by the dedicated r.Get above
			// (before CreateOrUpdate), so on a brand-new Deployment the Get
			// returns NotFound and prevPackagesHash stays "".
			if prevPackagesHash != packagesHash {
				switch {
				case prevPackagesHash == "" && packagesHash != "":
					log.Info("packages configured", "packageCount", len(cluster.Spec.Packages),
						"packagesHash", packagesHash)
					r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PackagesConfigured",
						"Configured %d package(s) on Deployment %q", len(cluster.Spec.Packages), deploy.Name)
				case prevPackagesHash != "" && packagesHash != "":
					log.Info("packages updated", "oldPackagesHash", prevPackagesHash,
						"packagesHash", packagesHash, "packageCount", len(cluster.Spec.Packages))
					r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PackagesUpdated",
						"Package set changed (hash %s -> %s); rolling restart triggered",
						prevPackagesHash, packagesHash)
				case prevPackagesHash != "" && packagesHash == "":
					log.Info("packages removed", "oldPackagesHash", prevPackagesHash)
					r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PackagesRemoved",
						"Removed all packages from Deployment %q", deploy.Name)
				}
			}
		}
	}

	// 7. Reconcile HorizontalPodAutoscaler — uses `as` and `deploy` from above.
	isAdmin := node.Spec.Type == v1alpha1.NodeTypeAdmin
	autoscalingRequested := as != nil && as.Enabled

	// Non-empty when cleanup found a foreign HPA at our owned name.
	var hpaCollisionName string

	// Resources whose Utilization metric is declared on the HPA but lacks a
	// matching container request. Drives the Degraded overlay below.
	var hpaMissingRequestResources []corev1.ResourceName

	// Gate A: admin-with-autoscaling. CEL rejects this for new objects;
	// this branch only fires for pre-CEL stored data. Emit on Generation
	// transition only.
	if isAdmin && autoscalingRequested {
		log.Info("ignoring autoscaling spec on admin node",
			"node", node.Name, "minReplicas", as.MinReplicas, "maxReplicas", as.MaxReplicas)
		if node.Status.ObservedGeneration < node.Generation {
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "AutoscalingIgnored",
				"Autoscaling spec is set but ignored: HPA is not supported on admin nodes")
		}
	}

	if !isAdmin && autoscalingRequested {
		desiredHPA := buildHPA(&cluster, &node, as)

		// Gate D: pre-flight check for HPA Utilization metrics whose
		// matching container request is unset. K8s does not enforce this at
		// admission, so we surface it before KCM's FailedGetResourceMetric.
		resolvedResources := resolveResources(&cluster, &node)
		for _, m := range desiredHPA.Spec.Metrics {
			if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
				continue
			}
			if m.Resource.Target.Type != autoscalingv2.UtilizationMetricType {
				continue
			}
			resName := m.Resource.Name
			if resolvedResources != nil {
				if q, ok := resolvedResources.Requests[resName]; ok && !q.IsZero() {
					continue
				}
			}
			hpaMissingRequestResources = append(hpaMissingRequestResources, resName)
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "HPAMissingResourceRequest",
				"HPA targets %s Utilization but spec.resources.requests.%s is unset on the node or cluster; HPA will be unable to scale",
				resName, resName)
		}

		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      desiredHPA.Name,
				Namespace: desiredHPA.Namespace,
			},
		}
		result, err = controllerutil.CreateOrUpdate(ctx, r.Client, hpa, func() error {
			hpa.Labels = mergeManagedLabels(hpa.Labels, desiredHPA.Labels)
			hpa.Spec = desiredHPA.Spec
			return controllerutil.SetControllerReference(&node, hpa, r.Scheme)
		})
		if err != nil {
			if res, handled, statusErr := r.handlePermanentWriteError(ctx, &node, err); handled {
				return res, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("failed to reconcile HPA: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("HPA reconciled", "operation", result, "name", hpa.Name)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "HPAReconciled",
				"HorizontalPodAutoscaler %q %s", hpa.Name, result)
		}

		// Gate B: surface KCM's AbleToScale/ScalingActive=False via Warning.
		// Age-grace skips fresh HPAs whose first KCM conditions are transient
		// (status.observedGeneration is unreliable on some KCMs, so we don't
		// use it). Per-(node, cond) cache gates on LastTransitionTime so the
		// 15s status churn doesn't spam events.
		if time.Since(hpa.CreationTimestamp.Time) > HPAHealthGracePeriod {
			// When Gate D fired this reconcile, KCM will emit
			// FailedGetResourceMetric for the same root cause — skip the
			// redundant Warning but still stamp the cache so a later
			// transition (user fixes requests, then breaks them again)
			// re-emits cleanly.
			suppressFailedGetResourceMetric := len(hpaMissingRequestResources) > 0
			for _, c := range hpa.Status.Conditions {
				if c.Status != corev1.ConditionFalse {
					continue
				}
				if c.Type != autoscalingv2.AbleToScale && c.Type != autoscalingv2.ScalingActive {
					continue
				}
				// ScalingDisabled is a legitimate paused state, not a failure.
				if c.Reason == "ScalingDisabled" {
					continue
				}
				if suppressFailedGetResourceMetric &&
					c.Type == autoscalingv2.ScalingActive &&
					c.Reason == "FailedGetResourceMetric" {
					r.hpaUnsatisfiableSeen.transition(hpaCondKey{NodeUID: node.UID, CondType: c.Type}, c.LastTransitionTime.Time)
					continue
				}
				key := hpaCondKey{NodeUID: node.UID, CondType: c.Type}
				if r.hpaUnsatisfiableSeen.transition(key, c.LastTransitionTime.Time) {
					r.Recorder.Eventf(&node, corev1.EventTypeWarning, "HPAUnsatisfiable",
						"HorizontalPodAutoscaler %q cannot scale: %s=%s reason=%s message=%q",
						hpa.Name, c.Type, c.Status, c.Reason, c.Message)
				}
			}
			r.hpaUnsatisfiableSeen.reap(node.UID, hpa.Status.Conditions)
		}
	} else {
		// Cleanup branch — autoscaling disabled or admin. Delete owned HPA
		// or surface a foreign collision via Degraded.
		var existingHPA autoscalingv2.HorizontalPodAutoscaler
		err := r.Get(ctx, client.ObjectKey{
			Name: OwnedResourceName(cluster.Name, node.Name), Namespace: node.Namespace,
		}, &existingHPA)
		switch {
		case err == nil && !metav1.IsControlledBy(&existingHPA, &node):
			log.Info("skipping HPA deletion, not owned by this node", "name", existingHPA.Name)
			hpaCollisionName = existingHPA.Name
			// Emit every reconcile; K8s server-side Event dedup folds repeats
			// into one Event with incrementing count and fresh lastTimestamp.
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "HPANotOwned",
				"HPA %q exists but is not managed by this node; skipping deletion", existingHPA.Name)
		case err == nil:
			if delErr := r.Delete(ctx, &existingHPA); delErr != nil && !apierrors.IsNotFound(delErr) {
				return ctrl.Result{}, fmt.Errorf("failed to delete HPA: %w", delErr)
			}
			log.Info("HPA deleted", "name", existingHPA.Name)
			// Gate A above already covered AutoscalingIgnored for the admin
			// case; emit Normal HPADeleted uniformly here.
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "HPADeleted",
				"HorizontalPodAutoscaler %q deleted", existingHPA.Name)
		case apierrors.IsNotFound(err):
			// No HPA exists — nothing to clean up.
		default:
			return ctrl.Result{}, fmt.Errorf("failed to get HPA: %w", err)
		}
	}

	// 8. Reconcile PodDisruptionBudget (runtime nodes only). CEL guarantees
	// exactly one of minAvailable/maxUnavailable is set when the spec is present.
	pdbSpec := resolvePDB(&cluster, &node)
	pdbRequested := pdbSpec != nil && (pdbSpec.MinAvailable != nil || pdbSpec.MaxUnavailable != nil)

	// Admin guard: log + event whenever PDB is requested on an admin node, regardless
	// of whether a stale PDB exists. K8s Event server-side dedup absorbs repeats.
	if isAdmin && pdbRequested {
		log.Info("ignoring PDB spec on admin node", "node", node.Name)
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
			existingPDB.Labels = mergeManagedLabels(existingPDB.Labels, desiredPDB.Labels)
			existingPDB.Spec = desiredPDB.Spec
			return controllerutil.SetControllerReference(&node, existingPDB, r.Scheme)
		})
		if err != nil {
			if res, handled, statusErr := r.handlePermanentWriteError(ctx, &node, err); handled {
				return res, statusErr
			}
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
		err := r.Get(ctx, client.ObjectKey{Name: OwnedResourceName(cluster.Name, node.Name), Namespace: node.Namespace}, &existingPDB)
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

	// 8b. Reconcile NetworkPolicy: admin nodes only, when cluster.Spec.NetworkPolicy
	// is set. Runtime nodes never own one.
	var npCollisionName string
	if isAdmin && cluster.Spec.NetworkPolicy != nil {
		desiredNP := buildNetworkPolicy(&cluster, &node)
		existingNP := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: desiredNP.Name, Namespace: desiredNP.Namespace},
		}
		result, err = controllerutil.CreateOrUpdate(ctx, r.Client, existingNP, func() error {
			existingNP.Labels = mergeManagedLabels(existingNP.Labels, desiredNP.Labels)
			existingNP.Spec = desiredNP.Spec
			return controllerutil.SetControllerReference(&node, existingNP, r.Scheme)
		})
		if err != nil {
			if res, handled, statusErr := r.handlePermanentWriteError(ctx, &node, err); handled {
				return res, statusErr
			}
			return ctrl.Result{}, fmt.Errorf("failed to reconcile NetworkPolicy: %w", err)
		}
		if result != controllerutil.OperationResultNone {
			log.Info("NetworkPolicy reconciled", "operation", result, "name", existingNP.Name)
			r.Recorder.Eventf(&node, corev1.EventTypeNormal, "NetworkPolicyReconciled",
				"NetworkPolicy %q %s", existingNP.Name, result)
		}
	} else {
		// Delete a stale operator-owned NetworkPolicy (networkPolicy cleared, or the
		// node flipped admin→runtime). Refuse to delete one we don't own; surface
		// that collision as Degraded.
		var existingNP networkingv1.NetworkPolicy
		err := r.Get(ctx, client.ObjectKey{Name: OwnedResourceName(cluster.Name, node.Name), Namespace: node.Namespace}, &existingNP)
		if err == nil {
			if !metav1.IsControlledBy(&existingNP, &node) {
				log.Info("skipping NetworkPolicy deletion, not owned by this node", "name", existingNP.Name)
				r.Recorder.Eventf(&node, corev1.EventTypeWarning, "NetworkPolicyNotOwned",
					"NetworkPolicy %q exists but is not managed by this node; skipping deletion", existingNP.Name)
				npCollisionName = existingNP.Name
			} else if delErr := r.Delete(ctx, &existingNP); delErr != nil && !apierrors.IsNotFound(delErr) {
				return ctrl.Result{}, fmt.Errorf("failed to delete NetworkPolicy: %w", delErr)
			} else {
				log.Info("NetworkPolicy deleted", "name", existingNP.Name)
				r.Recorder.Eventf(&node, corev1.EventTypeNormal, "NetworkPolicyDeleted",
					"NetworkPolicy %q deleted", existingNP.Name)
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("failed to get NetworkPolicy: %w", err)
		}
	}

	// 9. Compute status from Deployment.
	// Capture PackagesReady (which lives outside the K8s-Deployment-mirror
	// set) BEFORE computeNodeConditions rebuilds the slice — otherwise it
	// would be dropped on every reconcile and re-set only when the
	// translator/pre-check happens to fire (causing flap on each transient
	// pod-watch trigger). Take a value copy so the captured snapshot is
	// independent of the soon-to-be-replaced slice.
	var priorPackagesReady *metav1.Condition
	if c := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady); c != nil {
		copied := *c
		priorPackagesReady = &copied
	}

	conditions, rs := computeNodeConditions(deploy, node.Generation)
	node.Status.Conditions = conditions

	// Re-apply ClusterReady — computeNodeConditions rebuilds the slice from
	// Deployment health and would otherwise drop it.
	setCondition(&node.Status.Conditions, v1alpha1.ConditionClusterReady, metav1.ConditionTrue,
		v1alpha1.ReasonClusterFound, fmt.Sprintf("Referenced cluster %q found", cluster.Name), node.Generation)

	// Restore the prior PackagesReady value verbatim (including its
	// original LastTransitionTime) so the translator below can preserve
	// it across set=false reconciles. Bypass setCondition — that helper's
	// underlying apimeta.SetStatusCondition stamps a fresh
	// LastTransitionTime when the target condition is absent (which it
	// is here, since the slice was just replaced), and re-stamping every
	// reconcile would make the K8s "LastTransitionTime only on status
	// flip" convention a lie. Direct append is safe: the condition was
	// just confirmed absent in the new slice.
	if priorPackagesReady != nil {
		node.Status.Conditions = append(node.Status.Conditions, *priorPackagesReady)
	}

	// HPA/PDB Degraded overlays — applied in this order so the PDB collision
	// (applied last below) wins when a node has both.
	if hpaCollisionName != "" {
		setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"HPANotOwned",
			fmt.Sprintf("A HorizontalPodAutoscaler named %q exists but is not owned by this node", hpaCollisionName),
			node.Generation)
	}

	if len(hpaMissingRequestResources) > 0 {
		names := make([]string, len(hpaMissingRequestResources))
		for i, n := range hpaMissingRequestResources {
			names[i] = string(n)
		}
		joined := strings.Join(names, ", ")
		setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"HPAMissingResourceRequest",
			fmt.Sprintf("HPA targets %s Utilization but spec.resources.requests is unset for: %s — HPA will be unable to scale", joined, joined),
			node.Generation)
	}

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

	if npCollisionName != "" {
		setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
			"NetworkPolicyNotOwned",
			fmt.Sprintf("A NetworkPolicy named %q exists but is not owned by this node", npCollisionName),
			node.Generation)
	}

	// Pod-failure overlay: surface a stuck rollout's cause (bad image, crashloop,
	// config error) that computeNodeConditions hides behind Ready=True off the old
	// pod. Skipped when a collision overlay already set Degraded — those are more
	// actionable, don't clobber them.
	collisionActive := hpaCollisionName != "" || len(hpaMissingRequestResources) > 0 ||
		pdbCollisionName != "" || npCollisionName != ""
	if !collisionActive {
		if pods, listErr := r.listOwnedPods(ctx, cluster.Name, node.Name, node.Namespace); listErr != nil {
			log.Error(listErr, "failed to list pods for pod-failure overlay")
		} else if pf := translatePodFailure(pods); pf.failing {
			changed := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
				Type:               v1alpha1.ConditionDegraded,
				Status:             metav1.ConditionTrue,
				Reason:             "PodUnhealthy",
				Message:            pf.message,
				ObservedGeneration: node.Generation,
			})
			if changed {
				r.Recorder.Eventf(&node, corev1.EventTypeWarning, "PodUnhealthy", "%s", pf.message)
			}
		}
	}

	// PackagesReady overlay (translator leg). When packages are configured,
	// list the pods owned by this node's Deployment and derive PackagesReady
	// from observed init-container state. Pre-check fired earlier for the
	// spec-change path; this is the runtime arm that catches curl exit
	// codes, image-pull failures, and kubelet env-var Secret resolution
	// failures the cache pre-check missed.
	if len(cluster.Spec.Packages) > 0 {
		pods, listErr := r.listOwnedPods(ctx, cluster.Name, node.Name, node.Namespace)
		if listErr != nil {
			// Non-fatal — leave PackagesReady at whatever value the
			// previous reconcile (or pre-check) set; controller-runtime
			// will backoff on the returned error from a downstream step.
			log.Error(listErr, "failed to list pods for PackagesReady translator")
		} else if res := translatePackagesReady(pods, packagesHash, cluster.Spec.Packages); res.set {
			setCondition(&node.Status.Conditions, v1alpha1.ConditionPackagesReady,
				res.status, res.reason, res.message, node.Generation)
		}

		// ReplicaFailure override (R2). Translator only sees Pods that
		// exist; when the ReplicaSet controller cannot create pods at
		// all (SCC rejection on OpenShift, admission webhook denial,
		// ResourceQuota exhausted, invalid pod spec), no pod-watch
		// events fire and PackagesReady would otherwise stay at the
		// preemptive PackagesPending forever. Surface the Deployment's
		// ReplicaFailure condition's verbatim message under a new
		// PackagePodCreationFailed reason. Applied LAST so it
		// overrides whatever the translator produced — a blocked
		// new-spec rollout is more urgent than any observation of an
		// old still-serving pod.
		if msg, failing := deploymentReplicaFailureMessage(deploy); failing {
			setCondition(&node.Status.Conditions, v1alpha1.ConditionPackagesReady,
				metav1.ConditionFalse, v1alpha1.ReasonPackagePodCreationFailed, msg, node.Generation)
		}

		// Recovery action (Secret-watch path). When this reconcile was
		// triggered while PackagesReady was in a recoverable failure AND
		// the pre-check passed (we would have returned early if it did
		// not), delete the failing package-fetch pods so kubelet starts
		// a fresh container that re-resolves the Secret. Without this,
		// failing pods sit in CrashLoopBackOff with cached env values
		// for up to 5 min while the operator's pre-check is already
		// satisfied.
		if isRecoverableSecretFailure(priorPackagesReadyForGate) && listErr == nil {
			// Gate recovery on actual Secret changes. Compute a hash of
			// the resourceVersions of every referenced Secret; only
			// delete pods if the hash differs from what we stamped on
			// the last successful recovery. Without this, every
			// reconcile during a recoverable failure deletes the pod
			// even when the user's Secret value is still wrong — an
			// infinite delete/recreate loop.
			currentSecretsHash, hashErr := computePackageSecretsRVHash(ctx, r.Client, cluster.Spec.Packages, node.Namespace)
			if hashErr != nil {
				log.Error(hashErr, "computing package Secrets rv hash during recovery",
					"cluster", cluster.Name, "node", node.Name)
			} else if currentSecretsHash != "" && currentSecretsHash != node.Status.LastObservedPackageSecretsHash {
				deleted, delErr := r.deleteFailingPackagePods(ctx, pods, packagesHash)
				if delErr != nil {
					log.Error(delErr, "deleting failing package-fetch pods during recovery",
						"cluster", cluster.Name, "node", node.Name)
				}
				if deleted > 0 {
					r.Recorder.Eventf(&node, corev1.EventTypeNormal, "PackageRecoveryAttempted",
						"Deleted %d failing package-fetch pod(s) after Secret change; kubelet will restart with fresh credentials",
						deleted)
				}
				// Record the observed hash regardless of delete count
				// (deleted=0 means filter rejected, but we still saw
				// the Secret change — don't repeat the gate check).
				node.Status.LastObservedPackageSecretsHash = currentSecretsHash
			}
		}
	} else {
		// No packages configured — drop any stale PackagesReady from a
		// previous reconcile when packages WERE configured.
		apimeta.RemoveStatusCondition(&node.Status.Conditions, v1alpha1.ConditionPackagesReady)
	}

	// Ready overlay (plan F2a). After PackagesReady is set, if it is
	// False, force Ready=False with reason PackagesNotReady — Ready means
	// "the desired spec is being honored end to end," and a broken
	// package fetch breaks that contract even when the K8s Deployment
	// itself looks healthy (old pod still serving during failed rollout).
	applyReadyOverlay(&node.Status.Conditions, node.Generation)

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
		For(&v1alpha1.IdentityServerNode{},
			builder.WithPredicates(predicate.Or(
				predicate.GenerationChangedPredicate{},
				predicate.LabelChangedPredicate{},
				predicate.AnnotationChangedPredicate{},
			))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForCluster),
			builder.WithPredicates(clusterSpecOrNodeCountChangedPredicate{}),
		).
		Watches(
			&corev1.ConfigMap{},
			newUnionScopeHandler(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			newUnionScopeHandler(r.findNodesForManagedConfig),
			builder.WithPredicates(managedConfigPredicate{}),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForPod),
			builder.WithPredicates(packageInitContainerStatusChangedPredicate{}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForPackageSecret),
			builder.WithPredicates(packageSecretCandidatePredicate{}),
		).
		Complete(r)
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

// findNodesForPod maps a Pod event (Create / Update / Delete passed through
// packageInitContainerStatusChangedPredicate) to the owning IdentityServerNode
// by reverse-resolving the labels stamped on the Pod by buildLabels
// (resources.go:439). On Delete events the tombstone object preserves both
// labels and OwnerReferences, so the resolution path works identically for
// removals — the reconciler interprets a missing pod as a recovery signal.
// Skips OwnerReference traversal of the ReplicaSet→Deployment chain —
// those are two hops from the ISN, and the ReplicaSet lookup would require
// additional RBAC. The label-based path needs only the existing
// IdentityServerNode cache, and OwnedResourceName (resources.go:59) guarantees
// a unique mapping pod label → owning node.
//
// Defensive: missing/empty labels return nil rather than fanning out to every
// node in the namespace. Pods whose OwnerReference is not a ReplicaSet (hand-
// crafted, migrated workloads) are dropped — the contract is "pods produced by
// our Deployments only."
func (r *IdentityServerNodeReconciler) findNodesForPod(ctx context.Context, obj client.Object) []ctrl.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	// Skip ad-hoc / hand-crafted pods: the contract is pods produced by our
	// Deployments, which are owned by a controller ReplicaSet. Defensively
	// iterate OwnerReferences instead of indexing [0] — even though kubelet
	// sets the ReplicaSet as the controller in practice, an arbitrary index
	// is brittle if K8s ever changes ordering.
	if !hasReplicaSetController(pod.OwnerReferences) {
		return nil
	}

	instance := pod.Labels["curity.io/owned-by"]
	clusterName := pod.Labels["curity.io/cluster"]
	if instance == "" || clusterName == "" {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)
	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList,
		client.InNamespace(pod.Namespace),
		client.MatchingLabels{"curity.io/cluster": clusterName},
	); err != nil {
		log.Error(err, "failed to list nodes for pod watch", "pod", pod.Name, "cluster", clusterName)
		return nil
	}

	for i := range nodeList.Items {
		n := &nodeList.Items[i]
		if OwnedResourceName(clusterName, n.Name) == instance {
			return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(n)}}
		}
	}
	return nil
}

// findNodesForPackageSecret maps a Secret event to every IdentityServerNode
// whose parent cluster references the Secret from spec.packages. The watch
// fans out via this reverse-lookup because Secrets are user-owned and carry
// no operator label — we cannot pre-filter at the predicate layer. The
// predicate already trims out unrelated Secret types and cosmetic updates,
// so this function is invoked only for events that are *plausibly* relevant.
//
// Algorithm: list clusters in the Secret's namespace, keep the ones whose
// packages reference this Secret name, then enqueue every node belonging to
// each match (via the curity.io/cluster label). List errors are logged and
// produce an empty result — the watch is level-triggered, so a missed event
// is recovered by the next eligible Secret event or the existing 30 s
// requeue. The cache backs both Lists, so cost per event is O(clusters_in_ns
// + matched_clusters · nodes_per_cluster) with no API roundtrip.
func (r *IdentityServerNodeReconciler) findNodesForPackageSecret(ctx context.Context, obj client.Object) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret == nil {
		return nil
	}

	log := ctrl.LoggerFrom(ctx)
	var clusters v1alpha1.IdentityServerClusterList
	if err := r.List(ctx, &clusters, client.InNamespace(secret.Namespace)); err != nil {
		log.Error(err, "listing clusters for package-secret watch",
			"secret", secret.Name, "namespace", secret.Namespace)
		return nil
	}

	var requests []ctrl.Request
	for i := range clusters.Items {
		c := &clusters.Items[i]
		if !packageReferencesSecret(c.Spec.Packages, secret.Name) {
			continue
		}
		var nodes v1alpha1.IdentityServerNodeList
		if err := r.List(ctx, &nodes,
			client.InNamespace(secret.Namespace),
			client.MatchingLabels{"curity.io/cluster": c.Name},
		); err != nil {
			log.Error(err, "listing nodes for package-secret watch",
				"cluster", c.Name, "secret", secret.Name)
			continue
		}
		for j := range nodes.Items {
			n := &nodes.Items[j]
			requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(n)})
		}
	}
	return requests
}

// listOwnedPods returns the pods produced by this node's Deployment, filtered
// by the standard labels stamped on the pod template (resources.go:439).
// Used by the PackagesReady translator to derive a condition value from
// observed init-container state. Requires core/pods:list — the kubebuilder
// marker is on this reconciler. Returns the items slice directly so the
// caller can pass it to translatePackagesReady without an extra copy.
func (r *IdentityServerNodeReconciler) listOwnedPods(ctx context.Context, clusterName, nodeName, namespace string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app.kubernetes.io/managed-by": "curity-operator",
			"curity.io/owned-by":           OwnedResourceName(clusterName, nodeName),
		},
	); err != nil {
		return nil, fmt.Errorf("listing pods for node %q in cluster %q: %w", nodeName, clusterName, err)
	}
	return podList.Items, nil
}

// deleteFailingPackagePods iterates the already-listed pods (passed in by
// the caller to avoid a second cache List) and deletes those that match the
// failingPackagePodFilter for the given expectedHash. Returns the count of
// successfully-issued Delete calls. IsNotFound errors are tolerated — the
// pod is gone, which is the desired end state. Other errors are accumulated
// into the returned error so the caller can log but proceed.
//
// Uses direct client.Delete (not eviction). PDB only gates the
// pods/eviction subresource, so a strict PodDisruptionBudget will not
// block this path — verified empirically on OpenShift 2026-05-15.
func (r *IdentityServerNodeReconciler) deleteFailingPackagePods(
	ctx context.Context, pods []corev1.Pod, expectedHash string,
) (int, error) {
	log := ctrl.LoggerFrom(ctx)
	deleted := 0
	var errs []error
	policy := metav1.DeletePropagationBackground
	for i := range pods {
		if !failingPackagePodFilter(&pods[i], expectedHash) {
			continue
		}
		if err := r.Delete(ctx, &pods[i], &client.DeleteOptions{PropagationPolicy: &policy}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			log.Error(err, "deleting failing package-fetch pod", "pod", pods[i].Name)
			errs = append(errs, err)
			continue
		}
		log.Info("deleted failing package-fetch pod for recovery", "pod", pods[i].Name)
		deleted++
	}
	if len(errs) > 0 {
		return deleted, fmt.Errorf("%d delete error(s); first: %w", len(errs), errs[0])
	}
	return deleted, nil
}

// replicaFailureMessageMaxLen caps the verbatim K8s ReplicaFailure
// message embedded in the PackagesReady condition. K8s allows up to
// 32 KiB per condition message, but admission-webhook denials and CEL
// violation traces can be very long (~5-10 KiB). 4 KiB keeps the
// condition compact for `kubectl describe` consumers while leaving the
// actionable head of the diagnostic intact (stack traces tail off into
// less useful info, so we truncate from the end).
const replicaFailureMessageMaxLen = 4096

// truncateReplicaFailureMessage caps a verbatim K8s diagnostic at
// replicaFailureMessageMaxLen, appending a "(truncated, N more bytes)"
// indicator so users know to look at the source. Mirrors the shape of
// truncatePackageURL.
func truncateReplicaFailureMessage(s string) string {
	if len(s) <= replicaFailureMessageMaxLen {
		return s
	}
	return s[:replicaFailureMessageMaxLen] + fmt.Sprintf("... (truncated, %d more bytes; see `kubectl describe deployment` for the full text)", len(s)-replicaFailureMessageMaxLen)
}

// deploymentReplicaFailureMessage reports whether the Deployment's
// ReplicaSet controller is unable to create pods, returning the verbatim
// ReplicaFailure message when so. The K8s Deployment controller sets
// status.conditions[ReplicaFailure]=True when its underlying ReplicaSet
// returns errors from the API server — most commonly SCC rejection on
// OpenShift (the captured case: anyuid + seccomp + UID 0 is not
// allowed by any standard SCC), but also admission-webhook denial,
// ResourceQuota exhaustion, invalid pod spec, NodeSelector match
// failures, etc. Returns failing=false when the Deployment is healthy
// (no ReplicaFailure condition or Status=False/Unknown), nil, or has no
// conditions yet. The K8s message is truncated to
// replicaFailureMessageMaxLen so a CEL/admission-webhook stack trace
// cannot blow up the PackagesReady condition message; the diagnostic
// head stays intact and the user is pointed at the source for the rest.
func deploymentReplicaFailureMessage(deploy *appsv1.Deployment) (message string, failing bool) {
	if deploy == nil {
		return "", false
	}
	for i := range deploy.Status.Conditions {
		c := &deploy.Status.Conditions[i]
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return fmt.Sprintf("Deployment ReplicaFailure: %s", truncateReplicaFailureMessage(c.Message)), true
			}
			return fmt.Sprintf("Deployment ReplicaFailure (reason=%q): pods cannot be created", c.Reason), true
		}
	}
	return "", false
}

// applyReadyOverlay forces ConditionReady to False with reason
// PackagesNotReady when ConditionPackagesReady is False on the node. The
// detailed message lives on PackagesReady; Ready delegates so `kubectl get`
// consumers see at-a-glance that the desired spec is not being honored
// end-to-end (plan F2a). Other PackagesReady states (True or absent) leave
// Ready alone — the K8s-Deployment-mirror semantics from computeNodeConditions
// stand for non-package failures.
func applyReadyOverlay(conditions *[]metav1.Condition, generation int64) {
	pkgs := apimeta.FindStatusCondition(*conditions, v1alpha1.ConditionPackagesReady)
	if pkgs == nil || pkgs.Status != metav1.ConditionFalse {
		return
	}
	setCondition(conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
		v1alpha1.ReasonPackagesNotReady,
		"package fetch not ready; see PackagesReady condition",
		generation)
}

// hasReplicaSetController reports whether obj's OwnerReferences contains a
// controller-reference whose Kind is ReplicaSet. Iterates instead of indexing
// [0] so the function is robust to the ordering of owner references — the
// only design choice from the plan's remaining Unknown.
func hasReplicaSetController(ownerRefs []metav1.OwnerReference) bool {
	for i := range ownerRefs {
		ref := &ownerRefs[i]
		if ref.Controller != nil && *ref.Controller && ref.Kind == "ReplicaSet" {
			return true
		}
	}
	return false
}

// cleanupOrphanChildren deletes child resources owned by this node whose names
// don't match the current OwnedResourceName(clusterRef, node). This handles the
// case where a node's IdentityServerClusterRef has changed since prior children
// were created — e.g. an admin moves from cluster-a to cluster-b, leaving
// <cluster-a>-<node> resources that the standard CreateOrUpdate path no longer
// looks at because it operates on the new <cluster-b>-<node> name.
//
// Filtered by both managed-by label (skip foreign workloads in the namespace)
// and metav1.IsControlledBy (UID match — skip peer clusters' resources, even if
// a peer node shares this node's name). Idempotent: a steady-state reconcile
// with no orphans does four cache-backed Lists and exits without mutations.
//
// Deletes use Background propagation so cascade GC runs async; the order in
// which we issue the four kinds doesn't affect the actual deletion order.
func (r *IdentityServerNodeReconciler) cleanupOrphanChildren(
	ctx context.Context,
	node *v1alpha1.IdentityServerNode,
) error {
	log := ctrl.LoggerFrom(ctx)
	keep := OwnedResourceName(node.Spec.IdentityServerClusterRef.Name, node.Name)
	listOpts := []client.ListOption{
		client.InNamespace(node.Namespace),
		client.MatchingLabels{"app.kubernetes.io/managed-by": "curity-operator"},
	}
	var cleaned []string

	deleteIfOrphan := func(obj client.Object, kind string) error {
		if obj.GetName() == keep {
			return nil
		}
		if !metav1.IsControlledBy(obj, node) {
			return nil
		}
		if err := r.Delete(ctx, obj, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("delete orphan %s %q: %w", kind, obj.GetName(), err)
		}
		cleaned = append(cleaned, fmt.Sprintf("%s/%s", kind, obj.GetName()))
		return nil
	}

	var hpas autoscalingv2.HorizontalPodAutoscalerList
	if err := r.List(ctx, &hpas, listOpts...); err != nil {
		return fmt.Errorf("list HPAs for orphan cleanup: %w", err)
	}
	for i := range hpas.Items {
		if err := deleteIfOrphan(&hpas.Items[i], "HorizontalPodAutoscaler"); err != nil {
			return err
		}
	}

	var pdbs policyv1.PodDisruptionBudgetList
	if err := r.List(ctx, &pdbs, listOpts...); err != nil {
		return fmt.Errorf("list PDBs for orphan cleanup: %w", err)
	}
	for i := range pdbs.Items {
		if err := deleteIfOrphan(&pdbs.Items[i], "PodDisruptionBudget"); err != nil {
			return err
		}
	}

	var nps networkingv1.NetworkPolicyList
	if err := r.List(ctx, &nps, listOpts...); err != nil {
		return fmt.Errorf("list NetworkPolicies for orphan cleanup: %w", err)
	}
	for i := range nps.Items {
		if err := deleteIfOrphan(&nps.Items[i], "NetworkPolicy"); err != nil {
			return err
		}
	}

	var deploys appsv1.DeploymentList
	if err := r.List(ctx, &deploys, listOpts...); err != nil {
		return fmt.Errorf("list Deployments for orphan cleanup: %w", err)
	}
	for i := range deploys.Items {
		if err := deleteIfOrphan(&deploys.Items[i], "Deployment"); err != nil {
			return err
		}
	}

	var svcs corev1.ServiceList
	if err := r.List(ctx, &svcs, listOpts...); err != nil {
		return fmt.Errorf("list Services for orphan cleanup: %w", err)
	}
	for i := range svcs.Items {
		if err := deleteIfOrphan(&svcs.Items[i], "Service"); err != nil {
			return err
		}
	}

	if len(cleaned) > 0 {
		log.Info("orphan children cleaned", "node", node.Name, "currentName", keep, "removed", cleaned)
		r.Recorder.Eventf(node, corev1.EventTypeNormal, "OrphansCleaned",
			"Removed stale child resources from previous cluster reference: %s", strings.Join(cleaned, ", "))
	}
	return nil
}
