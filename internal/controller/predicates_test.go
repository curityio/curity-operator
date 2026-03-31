package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func TestNodePredicate_UpdateGenerationChangeOnly(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, nil),
		node(2, nil),
	)
	if !p.Update(e) {
		t.Error("expected pass when generation changed")
	}
}

func TestNodePredicate_UpdateConditionStatusChange(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "AllReady"},
		}),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "AllReady"},
		}),
	)
	if !p.Update(e) {
		t.Error("expected pass when condition status changed")
	}
}

func TestNodePredicate_UpdateConditionReasonChange(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "NotReady"},
		}),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: "ClusterNotFound"},
		}),
	)
	if !p.Update(e) {
		t.Error("expected pass when condition reason changed")
	}
}

func TestNodePredicate_UpdateConditionMessageChange(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R", Message: "old"},
		}),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R", Message: "new"},
		}),
	)
	if !p.Update(e) {
		t.Error("expected pass when condition message changed")
	}
}

func TestNodePredicate_UpdateConditionAdded(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, nil),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue},
		}),
	)
	if !p.Update(e) {
		t.Error("expected pass when condition added")
	}
}

func TestNodePredicate_UpdateConditionRemoved(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue},
		}),
		node(1, nil),
	)
	if !p.Update(e) {
		t.Error("expected pass when condition removed")
	}
}

func TestNodePredicate_UpdateOnlyLastTransitionTimeChanged(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	now := metav1.Now()
	later := metav1.NewTime(now.Add(time.Minute))
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R", LastTransitionTime: now},
		}),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R", LastTransitionTime: later},
		}),
	)
	if p.Update(e) {
		t.Error("expected block when only LastTransitionTime changed")
	}
}

func TestNodePredicate_UpdateConditionOrderChange(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
			{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "R"},
		}),
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue, Reason: "R"},
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
		}),
	)

	if !p.Update(e) {
		t.Error("expected pass when condition order changed (order-sensitive by design)")
	}
}

func TestNodePredicate_UpdateNoMaterialChange(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	e := updateEvent(
		node(1, conds),
		node(1, conds),
	)
	if p.Update(e) {
		t.Error("expected block when nothing material changed")
	}
}

func TestNodePredicate_UpdateBothGenerationAndConditionsChanged(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := updateEvent(
		node(1, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue},
		}),
		node(2, []metav1.Condition{
			{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse},
		}),
	)
	if !p.Update(e) {
		t.Error("expected pass when both generation and conditions changed")
	}
}

func TestNodePredicate_UpdateNonNodeObject(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	// Non-IdentityServerNode objects: extractConditions returns nil for both,
	// conditionsEqual(nil, nil) returns true, and generation is equal, so blocked.
	// This is correct — the predicate is only used on node watches.
	old := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	new := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if p.Update(e) {
		t.Error("expected block for non-node objects with same generation")
	}

	// But generation change still passes.
	new2 := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Generation: 2}}
	e2 := event.UpdateEvent{ObjectOld: old, ObjectNew: new2}
	if !p.Update(e2) {
		t.Error("expected pass for non-node objects with generation change")
	}
}

func TestNodePredicate_UpdateNilObjects(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := event.UpdateEvent{ObjectOld: nil, ObjectNew: nil}
	if !p.Update(e) {
		t.Error("expected pass when objects are nil (safety fallback)")
	}
}

func TestNodePredicate_CreateEvent(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := event.CreateEvent{Object: node(1, nil)}
	if !p.Create(e) {
		t.Error("expected pass for create events")
	}
}

func TestNodePredicate_DeleteEvent(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	e := event.DeleteEvent{Object: node(1, nil)}
	if !p.Delete(e) {
		t.Error("expected pass for delete events")
	}
}

// --- clusterSpecOrNodeCountChangedPredicate tests ---

func TestClusterPredicate_UpdateGenerationChange(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.UpdateEvent{
		ObjectOld: cluster(1, 0),
		ObjectNew: cluster(2, 0),
	}
	if !p.Update(e) {
		t.Error("expected pass when generation changed")
	}
}

func TestClusterPredicate_UpdateNodeCountChange(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.UpdateEvent{
		ObjectOld: cluster(1, 0),
		ObjectNew: cluster(1, 1),
	}
	if !p.Update(e) {
		t.Error("expected pass when NodeCount changed")
	}
}

func TestClusterPredicate_UpdateNoMaterialChange(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.UpdateEvent{
		ObjectOld: cluster(1, 2),
		ObjectNew: cluster(1, 2),
	}
	if p.Update(e) {
		t.Error("expected block when nothing material changed")
	}
}

func TestClusterPredicate_UpdateStatusOnlyNoNodeCountChange(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := cluster(1, 2)
	new := cluster(1, 2)
	new.Status.ReadyNodes = 1 // only ReadyNodes changed, not NodeCount
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if p.Update(e) {
		t.Error("expected block when only ReadyNodes changed")
	}
}

func TestClusterPredicate_UpdateNonClusterObject(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	new := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if !p.Update(e) {
		t.Error("expected pass for non-cluster objects (safety fallback)")
	}
}

func TestClusterPredicate_CreateEvent(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.CreateEvent{Object: cluster(1, 0)}
	if !p.Create(e) {
		t.Error("expected pass for create events")
	}
}

func TestClusterPredicate_DeleteEvent(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.DeleteEvent{Object: cluster(1, 0)}
	if !p.Delete(e) {
		t.Error("expected pass for delete events")
	}
}

func TestClusterPredicate_UpdateNilObjects(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	e := event.UpdateEvent{ObjectOld: nil, ObjectNew: nil}
	if !p.Update(e) {
		t.Error("expected pass when objects are nil (safety fallback)")
	}
}

// --- helpers ---

func node(generation int64, conditions []metav1.Condition) *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Generation: generation},
		Status:     v1alpha1.IdentityServerNodeStatus{Conditions: conditions},
	}
}

func updateEvent(old, new *v1alpha1.IdentityServerNode) event.UpdateEvent {
	return event.UpdateEvent{ObjectOld: old, ObjectNew: new}
}

func cluster(generation int64, nodeCount int32) *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Generation: generation},
		Status:     v1alpha1.IdentityServerClusterStatus{NodeCount: nodeCount},
	}
}
