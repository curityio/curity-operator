package controller

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// nodeStatusConditionsChangedPredicate passes update events when either:
//   - the object's generation changed (spec was modified), or
//   - the object's status conditions changed (Type, Status, Reason, or Message).
//
// Create, Delete, and Generic events always pass through.
//
// This is used by the cluster controller's watch on IdentityServerNode resources,
// where the cluster needs to react to node spec changes AND node status condition
// changes (for aggregating cluster-level health), but should ignore updates that
// only change non-material fields like resourceVersion or LastTransitionTime.
type nodeStatusConditionsChangedPredicate struct {
	predicate.Funcs
}

func (p nodeStatusConditionsChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}

	// Always pass through generation changes (spec was modified).
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		return true
	}

	// Check if status conditions changed.
	return !conditionsEqual(extractConditions(e.ObjectOld), extractConditions(e.ObjectNew))
}

// extractConditions returns the status conditions from an IdentityServerNode,
// or nil if the object is not an IdentityServerNode.
func extractConditions(obj client.Object) []metav1.Condition {
	if node, ok := obj.(*v1alpha1.IdentityServerNode); ok {
		return node.Status.Conditions
	}
	return nil
}

// clusterSpecOrNodeCountChangedPredicate passes update events when either:
//   - the object's generation changed (spec was modified), or
//   - the cluster's Status.NodeCount changed (a node was added/removed).
//
// Create, Delete, and Generic events always pass through.
//
// This is used by the node controller's watch on IdentityServerCluster resources.
// Nodes need to react to cluster spec changes (image, config, etc.) AND to
// peer node additions/removals (for duplicate admin/role detection), which are
// signalled by the cluster's NodeCount status field changing.
type clusterSpecOrNodeCountChangedPredicate struct {
	predicate.Funcs
}

func (p clusterSpecOrNodeCountChangedPredicate) Update(e event.UpdateEvent) bool {
	if e.ObjectOld == nil || e.ObjectNew == nil {
		return true
	}

	// Always pass through generation changes (spec was modified).
	if e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() {
		return true
	}

	// Check if NodeCount changed (a node was added or removed).
	oldCluster, oldOK := e.ObjectOld.(*v1alpha1.IdentityServerCluster)
	newCluster, newOK := e.ObjectNew.(*v1alpha1.IdentityServerCluster)
	if !oldOK || !newOK {
		return true
	}
	if oldCluster.Status.NodeCount != newCluster.Status.NodeCount {
		return true
	}

	// Check if the validated config hash annotation changed. This annotation
	// is set by the cluster reconciler when config validation succeeds, and
	// nodes need to re-reconcile to mount the newly validated configs.
	if e.ObjectOld.GetAnnotations()[annotationValidatedConfigHash] !=
		e.ObjectNew.GetAnnotations()[annotationValidatedConfigHash] {
		return true
	}

	// Check if ClusterConfigReady condition changed. Nodes gate Deployment
	// creation on this condition and need to reconcile when it transitions.
	if clusterConditionStatus(oldCluster, v1alpha1.ConditionClusterConfigReady) !=
		clusterConditionStatus(newCluster, v1alpha1.ConditionClusterConfigReady) {
		return true
	}

	return false
}

// managedConfigPredicate passes events for ConfigMaps/Secrets that have (or had)
// the curity.io/managed=true label. For updates, it checks both old and new objects
// so that label removal also triggers reconciliation (to un-mount the config).
type managedConfigPredicate struct {
	predicate.Funcs
}

func (p managedConfigPredicate) Create(e event.CreateEvent) bool {
	return hasManagedLabel(e.Object)
}

func (p managedConfigPredicate) Update(e event.UpdateEvent) bool {
	return hasManagedLabel(e.ObjectNew) || hasManagedLabel(e.ObjectOld)
}

func (p managedConfigPredicate) Delete(e event.DeleteEvent) bool {
	return hasManagedLabel(e.Object)
}

// hasManagedLabel returns true if the object carries curity.io/managed=true.
func hasManagedLabel(obj client.Object) bool {
	if obj == nil {
		return false
	}
	return obj.GetLabels()[LabelManagedConfig] == "true"
}

// clusterConditionStatus returns the Status field of the named condition, or
// "" if the condition is not present. Used by the cluster predicate to detect
// ClusterConfigReady transitions.
func clusterConditionStatus(c *v1alpha1.IdentityServerCluster, condType string) metav1.ConditionStatus {
	for _, cond := range c.Status.Conditions {
		if cond.Type == condType {
			return cond.Status
		}
	}
	return ""
}

// conditionsEqual returns true if two condition slices are semantically equal.
// It compares Type, Status, Reason, and Message — intentionally skipping
// LastTransitionTime and ObservedGeneration, which change on every reconcile
// even when the logical condition state is unchanged.
func conditionsEqual(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Type != b[i].Type ||
			a[i].Status != b[i].Status ||
			a[i].Reason != b[i].Reason ||
			a[i].Message != b[i].Message {
			return false
		}
	}
	return true
}
