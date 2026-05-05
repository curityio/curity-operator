package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
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

func TestNodePredicate_UpdateControllerOwnerRefChanged(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	e := updateEvent(
		nodeWithOwnerUID(1, conds, "uid-a"),
		nodeWithOwnerUID(1, conds, "uid-b"),
	)
	if !p.Update(e) {
		t.Error("expected pass when controller-ownerRef UID changed")
	}
}

func TestNodePredicate_UpdateControllerOwnerRefAddedFromNil(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	e := updateEvent(
		nodeWithOwnerUID(1, conds, ""),
		nodeWithOwnerUID(1, conds, "uid-a"),
	)
	if !p.Update(e) {
		t.Error("expected pass when controller-ownerRef appeared on the node")
	}
}

func TestNodePredicate_UpdateOwnerRefSameUIDIsBlocked(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	e := updateEvent(
		nodeWithOwnerUID(1, conds, "uid-a"),
		nodeWithOwnerUID(1, conds, "uid-a"),
	)
	if p.Update(e) {
		t.Error("expected block when ownerRef UID is unchanged and nothing else material changed")
	}
}

func TestNodePredicate_UpdateOwnerRefRemoved(t *testing.T) {
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	e := updateEvent(
		nodeWithOwnerUID(1, conds, "uid-a"),
		nodeWithOwnerUID(1, conds, ""),
	)
	if !p.Update(e) {
		t.Error("expected pass when controller-ownerRef was removed (non-nil → nil)")
	}
}

func TestNodePredicate_UpdateNonControllerOwnerRefIsIgnored(t *testing.T) {
	// Adding/changing a *non-controller* ownerRef must not wake the cluster up.
	// metav1.GetControllerOf only returns the controller ref, so a stray
	// non-controller ref change should be invisible to the predicate.
	p := nodeStatusConditionsChangedPredicate{}
	conds := []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue, Reason: "R"},
	}
	old := nodeWithOwnerUID(1, conds, "uid-a")
	new := nodeWithOwnerUID(1, conds, "uid-a")
	new.OwnerReferences = append(new.OwnerReferences, metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       "some-deploy",
		UID:        "uid-deploy",
		// no Controller flag → non-controller
	})
	if p.Update(updateEvent(old, new)) {
		t.Error("expected block when only a non-controller ownerRef changed")
	}
}

func TestControllerOwnerRefChanged_BothNil(t *testing.T) {
	old := node(1, nil)
	new := node(1, nil)
	if controllerOwnerRefChanged(old, new) {
		t.Error("expected false when both controller refs are nil")
	}
}

func TestControllerOwnerRefChanged_SameUIDDifferentName(t *testing.T) {
	// Same UID, different Name in the OwnerRef. Defensive: UID is the
	// canonical identity for cascade purposes, so this should NOT trigger.
	old := nodeWithOwnerUID(1, nil, "uid-a")
	new := nodeWithOwnerUID(1, nil, "uid-a")
	new.OwnerReferences[0].Name = "renamed-cluster"
	if controllerOwnerRefChanged(old, new) {
		t.Error("expected false: same UID but different Name should not register as a controller change")
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

func TestClusterPredicate_UpdateClusterConfigReadyAbsentToPresent(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := cluster(1, 2)
	// Old has no condition; new has ConditionFalse ("" != "False" → should pass).
	new := cluster(1, 2)
	new.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionClusterConfigReady, Status: metav1.ConditionFalse, Reason: "JobCreated"},
	}
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if !p.Update(e) {
		t.Error("expected pass when ClusterConfigReady appears for the first time")
	}
}

func TestClusterPredicate_UpdateClusterConfigReadyBothAbsent(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := cluster(1, 2)
	new := cluster(1, 2)
	// Neither cluster has the ClusterConfigReady condition — should block.
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if p.Update(e) {
		t.Error("expected block when ClusterConfigReady absent on both old and new")
	}
}

func TestClusterPredicate_UpdateClusterConfigReadyChanged(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := cluster(1, 2)
	old.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionClusterConfigReady, Status: metav1.ConditionFalse, Reason: "JobRunning"},
	}
	new := cluster(1, 2)
	new.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionClusterConfigReady, Status: metav1.ConditionTrue, Reason: "SecretReady"},
	}
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if !p.Update(e) {
		t.Error("expected pass when ClusterConfigReady status changed")
	}
}

func TestClusterPredicate_UpdateClusterConfigReadyUnchanged(t *testing.T) {
	p := clusterSpecOrNodeCountChangedPredicate{}
	old := cluster(1, 2)
	old.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionClusterConfigReady, Status: metav1.ConditionFalse, Reason: "JobRunning"},
	}
	new := cluster(1, 2)
	new.Status.Conditions = []metav1.Condition{
		{Type: v1alpha1.ConditionClusterConfigReady, Status: metav1.ConditionFalse, Reason: "JobCreated"},
	}
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: new}
	if p.Update(e) {
		t.Error("expected block when ClusterConfigReady status unchanged (only reason changed)")
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

func nodeWithOwnerUID(generation int64, conditions []metav1.Condition, ownerUID string) *v1alpha1.IdentityServerNode {
	n := node(generation, conditions)
	if ownerUID != "" {
		n.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "IdentityServerCluster",
			Name:       "owner",
			UID:        types.UID(ownerUID),
			Controller: ptr.To(true),
		}}
	}
	return n
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

// --- managedConfigPredicate ---

func configMapWithLabels(labels map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "test-cm",
			Labels: labels,
		},
	}
}

func TestManagedConfigPredicate_CreateWithLabel(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.CreateEvent{Object: configMapWithLabels(map[string]string{LabelManagedConfig: "true"})}
	if !p.Create(e) {
		t.Error("expected pass for labeled ConfigMap create")
	}
}

func TestManagedConfigPredicate_CreateWithoutLabel(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.CreateEvent{Object: configMapWithLabels(nil)}
	if p.Create(e) {
		t.Error("expected filter for unlabeled ConfigMap create")
	}
}

func TestManagedConfigPredicate_UpdateDataChange(t *testing.T) {
	p := managedConfigPredicate{}
	managed := map[string]string{LabelManagedConfig: "true"}
	e := event.UpdateEvent{
		ObjectOld: configMapWithLabels(managed),
		ObjectNew: configMapWithLabels(managed),
	}
	if !p.Update(e) {
		t.Error("expected pass for labeled ConfigMap update")
	}
}

func TestManagedConfigPredicate_UpdateLabelAdded(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.UpdateEvent{
		ObjectOld: configMapWithLabels(nil),
		ObjectNew: configMapWithLabels(map[string]string{LabelManagedConfig: "true"}),
	}
	if !p.Update(e) {
		t.Error("expected pass when managed label added")
	}
}

func TestManagedConfigPredicate_UpdateLabelRemoved(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.UpdateEvent{
		ObjectOld: configMapWithLabels(map[string]string{LabelManagedConfig: "true"}),
		ObjectNew: configMapWithLabels(nil),
	}
	if !p.Update(e) {
		t.Error("expected pass when managed label removed (to un-mount)")
	}
}

func TestManagedConfigPredicate_UpdateNoLabel(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.UpdateEvent{
		ObjectOld: configMapWithLabels(nil),
		ObjectNew: configMapWithLabels(nil),
	}
	if p.Update(e) {
		t.Error("expected filter when neither old nor new has label")
	}
}

func TestManagedConfigPredicate_DeleteWithLabel(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.DeleteEvent{Object: configMapWithLabels(map[string]string{LabelManagedConfig: "true"})}
	if !p.Delete(e) {
		t.Error("expected pass for labeled ConfigMap delete")
	}
}

func TestManagedConfigPredicate_DeleteWithoutLabel(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.DeleteEvent{Object: configMapWithLabels(nil)}
	if p.Delete(e) {
		t.Error("expected filter for unlabeled ConfigMap delete")
	}
}

func TestManagedConfigPredicate_CreateNilObject(t *testing.T) {
	p := managedConfigPredicate{}
	e := event.CreateEvent{Object: nil}
	if p.Create(e) {
		t.Error("expected filter for nil object")
	}
}
