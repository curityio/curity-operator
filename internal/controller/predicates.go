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
	return oldCluster.Status.NodeCount != newCluster.Status.NodeCount
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
