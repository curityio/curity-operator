package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	// 6. Validate configuration paths before building Deployment
	if cluster.Spec.Configuration != nil {
		if err := validateConfigurationPaths(&cluster.Spec.Configuration.ValueFrom); err != nil {
			setCondition(&node.Status.Conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue,
				"InvalidConfiguration", err.Error(), node.Generation)
			setCondition(&node.Status.Conditions, v1alpha1.ConditionReady, metav1.ConditionFalse,
				"InvalidConfiguration", err.Error(), node.Generation)
			node.Status.ObservedGeneration = node.Generation
			if statusErr := r.Status().Update(ctx, &node); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("failed to update status: %w", statusErr)
			}
			r.Recorder.Eventf(&node, corev1.EventTypeWarning, "InvalidConfiguration",
				"Configuration validation failed: %s", err.Error())
			return ctrl.Result{}, nil
		}
	}

	// 7. Build and reconcile the Deployment
	desiredDeploy := buildDeployment(&cluster, &node)

	// Inject cluster config hash annotation for rolling restart when Secret changes
	configSecretName := cluster.Name + "-cluster-config"
	var configSecret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Name: configSecretName, Namespace: node.Namespace}, &configSecret); err == nil {
		if data, ok := configSecret.Data["cluster.xml"]; ok && len(data) > 0 {
			h := sha256.Sum256(data)
			if desiredDeploy.Spec.Template.Annotations == nil {
				desiredDeploy.Spec.Template.Annotations = make(map[string]string)
			}
			desiredDeploy.Spec.Template.Annotations["curity.io/cluster-config-hash"] = hex.EncodeToString(h[:])
		}
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: desiredDeploy.Name, Namespace: desiredDeploy.Namespace}}

	result, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = desiredDeploy.Labels
		deploy.Spec = desiredDeploy.Spec
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

	// 7. Build and reconcile the Service
	desiredSvc := buildService(&cluster, &node)
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: desiredSvc.Name, Namespace: desiredSvc.Namespace}}

	result, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desiredSvc.Labels
		svc.Spec.Type = desiredSvc.Spec.Type
		svc.Spec.Selector = desiredSvc.Spec.Selector
		svc.Spec.Ports = desiredSvc.Spec.Ports
		return controllerutil.SetControllerReference(&node, svc, r.Scheme)
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile Service: %w", err)
	}
	if result != controllerutil.OperationResultNone {
		log.Info("Service reconciled", "operation", result, "name", svc.Name)
		r.Recorder.Eventf(&node, corev1.EventTypeNormal, "ServiceReconciled",
			"Service %q %s", svc.Name, result)
	}

	// 8. Compute status from Deployment
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

	// 9. Update status
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
		Watches(
			&v1alpha1.IdentityServerCluster{},
			handler.EnqueueRequestsFromMapFunc(r.findNodesForCluster),
			builder.WithPredicates(clusterSpecOrNodeCountChangedPredicate{}),
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

	var nodeList v1alpha1.IdentityServerNodeList
	if err := r.List(ctx, &nodeList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{"curity.io/cluster": cluster.Name},
	); err != nil {
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
