package controller

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// =========================================================================
// packageInitContainerStatusChangedPredicate
// =========================================================================

// curityPod returns a *corev1.Pod with the operator's managed-by label
// stamped and the given init container statuses installed.
func curityPod(initStatuses []corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "curity-rt-abcd",
			Namespace: "ns",
			Labels: map[string]string{
				labelManagedBy: labelManagedByCurityOperator,
			},
		},
		Status: corev1.PodStatus{InitContainerStatuses: initStatuses},
	}
}

// waitingInitStatus builds a single InitContainerStatus in the Waiting state.
func waitingInitStatus(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}
}

// terminatedInitStatus builds a single InitContainerStatus in the Terminated state.
func terminatedInitStatus(name string, exitCode int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}},
	}
}

func TestPackageInitPredicate_CreateAlwaysFalse(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	pod := curityPod([]corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	})
	if p.Create(event.CreateEvent{Object: pod}) {
		t.Error("Create must be filtered out — kubelet has not yet populated status; wait for Update")
	}
}

func TestPackageInitPredicate_GenericAlwaysFalse(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	pod := curityPod(nil)
	if p.Generic(event.GenericEvent{Object: pod}) {
		t.Error("Generic must be filtered out")
	}
}

func TestPackageInitPredicate_DeleteAlwaysTrue(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	pod := curityPod(nil)
	if !p.Delete(event.DeleteEvent{Object: pod}) {
		t.Error("Delete must pass — pod removal is a recovery signal")
	}
}

func TestPackageInitPredicate_UpdateLabelMismatchDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	stranger := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "stranger", Namespace: "ns"}}
	oldP := stranger.DeepCopy()
	newP := stranger.DeepCopy()
	newP.Status.InitContainerStatuses = []corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 60),
	}
	if p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("pods without managed-by=curity-operator must be filtered out")
	}
}

func TestPackageInitPredicate_UpdateNoPackageFetchContainerDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	// pod has init container, but not one whose name starts with package-fetch-
	oldP := curityPod([]corev1.ContainerStatus{
		{Name: "config-discovery", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	})
	newP := curityPod([]corev1.ContainerStatus{
		{Name: "config-discovery", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	})
	if p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("pod without any package-fetch-* init container must be filtered out")
	}
}

func TestPackageInitPredicate_UpdateStatusUnchangedDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	st := terminatedInitStatus("package-fetch-0", 0)
	oldP := curityPod([]corev1.ContainerStatus{st})
	newP := curityPod([]corev1.ContainerStatus{st})
	if p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("identical init-container status must be filtered out — protects against resourceVersion-only updates")
	}
}

func TestPackageInitPredicate_UpdateWaitingReasonChangePasses(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	oldP := curityPod([]corev1.ContainerStatus{waitingInitStatus("package-fetch-0", "PodInitializing", "")})
	newP := curityPod([]corev1.ContainerStatus{waitingInitStatus("package-fetch-0", "CreateContainerConfigError", `secret "x" not found`)})
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("waiting.reason transition from PodInitializing to CreateContainerConfigError must pass")
	}
}

func TestPackageInitPredicate_UpdateExitCodeChangePasses(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	oldP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 0)})
	newP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 60)})
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("terminated.exitCode change must pass — this is the headline failure signal")
	}
}

func TestPackageInitPredicate_UpdateRestartCountChangePasses(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	oldP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 60)})
	newP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 60)})
	newP.Status.InitContainerStatuses[0].RestartCount = 3
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("restartCount change must pass — CrashLoopBackOff is repeated failure")
	}
}

func TestPackageInitPredicate_UpdateAddedContainerPasses(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	oldP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 0)})
	newP := curityPod([]corev1.ContainerStatus{
		terminatedInitStatus("package-fetch-0", 0),
		terminatedInitStatus("package-fetch-1", 22),
	})
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("a new package-fetch-* status entry appearing must pass")
	}
}

func TestPackageInitPredicate_UpdateNilOldOrNewDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	pod := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 60)})
	if p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: pod}) {
		t.Error("nil ObjectOld must be filtered out")
	}
	if p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: nil}) {
		t.Error("nil ObjectNew must be filtered out")
	}
}

func TestPackageInitPredicate_UpdateNonPodObjectDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", Labels: map[string]string{labelManagedBy: labelManagedByCurityOperator}}}
	if p.Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: cm}) {
		t.Error("non-Pod object must be filtered out even if it carries the operator's managed-by label")
	}
}

// Running-vs-Running counts as "same" by design — the translator ignores
// containers in Running state (they are either healthy startup or hung,
// neither of which is a definitive failure). Verify the predicate does
// not wake the reconciler on cosmetic Running→Running ticks.
func TestPackageInitPredicate_UpdateRunningToRunningDrops(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	startedOld := metav1.NewTime(time.Now().Add(-10 * time.Second))
	startedNew := metav1.NewTime(time.Now())
	oldP := curityPod([]corev1.ContainerStatus{
		{Name: "package-fetch-0", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: startedOld}}},
	})
	newP := curityPod([]corev1.ContainerStatus{
		{Name: "package-fetch-0", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: startedNew}}},
	})
	if p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("Running→Running (different StartedAt) must be filtered out — translator ignores running entries")
	}
}

// State transitions across Waiting/Running/Terminated must trigger an update —
// they represent kubelet progress (or regression) the translator cares about.
func TestPackageInitPredicate_UpdateStateTransitionPasses(t *testing.T) {
	p := packageInitContainerStatusChangedPredicate{}
	oldP := curityPod([]corev1.ContainerStatus{waitingInitStatus("package-fetch-0", "PodInitializing", "")})
	newP := curityPod([]corev1.ContainerStatus{terminatedInitStatus("package-fetch-0", 0)})
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("Waiting → Terminated transition must pass")
	}
}

// --- packageSecretCandidatePredicate ---

func secretOpaque(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func TestPackageSecretCandidatePredicate_Update_NoChange(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v")})
	b := secretOpaque(map[string][]byte{"k": []byte("v")})
	if p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error("identical .Data: want false")
	}
}

func TestPackageSecretCandidatePredicate_Update_LabelOnly(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v")})
	b := secretOpaque(map[string][]byte{"k": []byte("v")})
	b.Labels = map[string]string{"new": "label"}
	if p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error("label-only change: want false")
	}
}

func TestPackageSecretCandidatePredicate_Update_DataValueChanged(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v1")})
	b := secretOpaque(map[string][]byte{"k": []byte("v2")})
	if !p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error(".Data value change: want true")
	}
}

func TestPackageSecretCandidatePredicate_Update_DataKeyAdded(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v")})
	b := secretOpaque(map[string][]byte{"k": []byte("v"), "k2": []byte("v2")})
	if !p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error(".Data key add: want true")
	}
}

func TestPackageSecretCandidatePredicate_Update_DataKeyRemoved(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v"), "k2": []byte("v2")})
	b := secretOpaque(map[string][]byte{"k": []byte("v")})
	if !p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error(".Data key remove: want true")
	}
}

func TestPackageSecretCandidatePredicate_Update_TypeChanged(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v")})
	b := secretOpaque(map[string][]byte{"k": []byte("v")})
	b.Type = corev1.SecretTypeTLS
	if !p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error("type change to acceptable: want true")
	}
}

func TestPackageSecretCandidatePredicate_Update_TypeChangedToUnsupported(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	a := secretOpaque(map[string][]byte{"k": []byte("v")})
	b := secretOpaque(map[string][]byte{"k": []byte("v")})
	b.Type = corev1.SecretTypeServiceAccountToken
	if p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}) {
		t.Error("type change to SA token: want false (new type rejected)")
	}
}

func TestPackageSecretCandidatePredicate_Update_NilOldOrNew(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	if p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: secretOpaque(nil)}) {
		t.Error("nil old: want false")
	}
	if p.Update(event.UpdateEvent{ObjectOld: secretOpaque(nil), ObjectNew: nil}) {
		t.Error("nil new: want false")
	}
}

func TestPackageSecretCandidatePredicate_Update_NonSecretType(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm"}}
	if p.Update(event.UpdateEvent{ObjectOld: cm, ObjectNew: cm}) {
		t.Error("non-Secret object: want false (defensive)")
	}
}

func TestPackageSecretCandidatePredicate_CreateAndDelete_TypeFilter(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	cases := []struct {
		name string
		typ  corev1.SecretType
		want bool
	}{
		{"empty (defaults to Opaque)", "", true},
		{"opaque", corev1.SecretTypeOpaque, true},
		{"tls", corev1.SecretTypeTLS, true},
		{"basic-auth", corev1.SecretTypeBasicAuth, true},
		{"service-account-token", corev1.SecretTypeServiceAccountToken, false},
		{"dockercfg", corev1.SecretTypeDockercfg, false},
		{"bootstrap-token", corev1.SecretTypeBootstrapToken, false},
	}
	for _, tc := range cases {
		s := &corev1.Secret{Type: tc.typ}
		if got := p.Create(event.CreateEvent{Object: s}); got != tc.want {
			t.Errorf("Create %s: want %v, got %v", tc.name, tc.want, got)
		}
		if got := p.Delete(event.DeleteEvent{Object: s}); got != tc.want {
			t.Errorf("Delete %s: want %v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestPackageSecretCandidatePredicate_Generic(t *testing.T) {
	p := packageSecretCandidatePredicate{}
	if p.Generic(event.GenericEvent{Object: secretOpaque(nil)}) {
		t.Error("generic event: want false")
	}
}

// ============================================================================
// jobFailedCreateEventPredicate (Event watch for genclust Job FailedCreate)
// ============================================================================

func makeFailedCreateTestEvent(eventType, reason, involvedKind string) *corev1.Event {
	return &corev1.Event{
		Type:           eventType,
		Reason:         reason,
		InvolvedObject: corev1.ObjectReference{Kind: involvedKind, Name: "n", Namespace: "ns"},
	}
}

func TestJobFailedCreateEventPredicate(t *testing.T) {
	p := jobFailedCreateEventPredicate{}

	cases := []struct {
		name       string
		obj        client.Object
		wantCreate bool
		wantUpdate bool
	}{
		{
			name:       "Warning FailedCreate on Job => pass",
			obj:        makeFailedCreateTestEvent(corev1.EventTypeWarning, "FailedCreate", "Job"),
			wantCreate: true,
			wantUpdate: true,
		},
		{
			name: "Normal FailedCreate on Job => drop",
			obj:  makeFailedCreateTestEvent(corev1.EventTypeNormal, "FailedCreate", "Job"),
		},
		{
			name: "Warning SuccessfulCreate on Job => drop",
			obj:  makeFailedCreateTestEvent(corev1.EventTypeWarning, "SuccessfulCreate", "Job"),
		},
		{
			name: "Warning FailedCreate on Pod (not Job) => drop",
			obj:  makeFailedCreateTestEvent(corev1.EventTypeWarning, "FailedCreate", "Pod"),
		},
		{
			name: "Warning FailedCreate on ReplicaSet => drop",
			obj:  makeFailedCreateTestEvent(corev1.EventTypeWarning, "FailedCreate", "ReplicaSet"),
		},
		{
			name: "wrong type (Pod) => drop",
			obj:  &corev1.Pod{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.Create(event.CreateEvent{Object: tc.obj}); got != tc.wantCreate {
				t.Errorf("Create=%v want=%v", got, tc.wantCreate)
			}
			if got := p.Update(event.UpdateEvent{ObjectOld: tc.obj, ObjectNew: tc.obj}); got != tc.wantUpdate {
				t.Errorf("Update=%v want=%v", got, tc.wantUpdate)
			}
		})
	}

	// Delete and Generic are always false.
	if p.Delete(event.DeleteEvent{Object: makeFailedCreateTestEvent(corev1.EventTypeWarning, "FailedCreate", "Job")}) {
		t.Error("Delete must always be false")
	}
	if p.Generic(event.GenericEvent{Object: makeFailedCreateTestEvent(corev1.EventTypeWarning, "FailedCreate", "Job")}) {
		t.Error("Generic must always be false")
	}
}

// ============================================================================
// clusterConfigPodChangedPredicate (Pod watch for genclust Job pods)
// ============================================================================

func clusterConfigPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{"curity.io/component": "cluster-config"},
		},
	}
}

func TestClusterConfigPodChangedPredicate_LabelGate(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	unlabeled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	if p.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled}) {
		t.Error("unlabeled Pod Update must drop")
	}
	if p.Delete(event.DeleteEvent{Object: unlabeled}) {
		t.Error("unlabeled Pod Delete must drop")
	}
}

func TestClusterConfigPodChangedPredicate_CreateAlwaysDrops(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	if p.Create(event.CreateEvent{Object: clusterConfigPod("p")}) {
		t.Error("Create must always drop (no status yet)")
	}
}

func TestClusterConfigPodChangedPredicate_DeletePassesWhenLabeled(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	if !p.Delete(event.DeleteEvent{Object: clusterConfigPod("p")}) {
		t.Error("Delete must pass for labeled pod")
	}
}

func TestClusterConfigPodChangedPredicate_UpdatePhaseChange(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	oldP := clusterConfigPod("p")
	oldP.Status.Phase = corev1.PodPending
	newP := clusterConfigPod("p")
	newP.Status.Phase = corev1.PodRunning
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("phase change must pass")
	}
}

func TestClusterConfigPodChangedPredicate_UpdateContainerStatusChange(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	oldP := clusterConfigPod("p")
	newP := clusterConfigPod("p")
	newP.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  "genclust",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("container waiting state change must pass")
	}
}

func TestClusterConfigPodChangedPredicate_UpdateIdentical(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	pod := clusterConfigPod("p")
	pod.Status.Phase = corev1.PodRunning
	if p.Update(event.UpdateEvent{ObjectOld: pod, ObjectNew: pod}) {
		t.Error("identical Update must drop")
	}
}

func TestClusterConfigPodChangedPredicate_UpdatePodScheduledTransition(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	oldP := clusterConfigPod("p")
	oldP.Status.Phase = corev1.PodPending
	newP := clusterConfigPod("p")
	newP.Status.Phase = corev1.PodPending
	newP.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: newP}) {
		t.Error("PodScheduled condition change must pass")
	}
}

func TestClusterConfigPodChangedPredicate_NilGuards(t *testing.T) {
	p := clusterConfigPodChangedPredicate{}
	if p.Update(event.UpdateEvent{ObjectOld: nil, ObjectNew: clusterConfigPod("p")}) {
		t.Error("nil ObjectOld must drop")
	}
	if p.Update(event.UpdateEvent{ObjectOld: clusterConfigPod("p"), ObjectNew: nil}) {
		t.Error("nil ObjectNew must drop")
	}
}

// ============================================================================
// databaseInitPodChangedPredicate (Pod watch for database-init Job pods)
// ============================================================================

func databaseInitPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels:    map[string]string{componentLabel: databaseComponentValue},
		},
	}
}

func TestDatabaseInitPodChangedPredicate_LabelGateAndCreate(t *testing.T) {
	p := databaseInitPodChangedPredicate{}
	unlabeled := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"}}
	if p.Update(event.UpdateEvent{ObjectOld: unlabeled, ObjectNew: unlabeled}) {
		t.Error("unlabeled Pod Update must drop")
	}
	if p.Delete(event.DeleteEvent{Object: unlabeled}) {
		t.Error("unlabeled Pod Delete must drop")
	}
	if p.Create(event.CreateEvent{Object: databaseInitPod("p")}) {
		t.Error("Create must always drop (no status yet)")
	}
	if !p.Delete(event.DeleteEvent{Object: databaseInitPod("p")}) {
		t.Error("Delete must pass for labeled pod")
	}
}

func TestDatabaseInitPodChangedPredicate_UpdateDiff(t *testing.T) {
	p := databaseInitPodChangedPredicate{}

	// Identical pods → drop (no kubelet-heartbeat churn).
	same := databaseInitPod("p")
	same.Status.Phase = corev1.PodPending
	if p.Update(event.UpdateEvent{ObjectOld: same, ObjectNew: same}) {
		t.Error("identical Update must drop")
	}

	// Phase change → pass.
	oldP := databaseInitPod("p")
	oldP.Status.Phase = corev1.PodPending
	running := databaseInitPod("p")
	running.Status.Phase = corev1.PodRunning
	if !p.Update(event.UpdateEvent{ObjectOld: oldP, ObjectNew: running}) {
		t.Error("phase change must pass")
	}

	// Init container waiting-state change → pass.
	stuck := databaseInitPod("p")
	stuck.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  databaseContainerName,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}
	if !p.Update(event.UpdateEvent{ObjectOld: databaseInitPod("p"), ObjectNew: stuck}) {
		t.Error("init container waiting-state change must pass")
	}
}

func opaqueSecret(name string, data map[string][]byte, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

func TestConnectionSecretChangedPredicate(t *testing.T) {
	p := connectionSecretChangedPredicate{}

	// Non-Opaque (e.g. SA token) is dropped.
	tok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "ns"}, Type: corev1.SecretTypeServiceAccountToken}
	if p.Create(event.CreateEvent{Object: tok}) {
		t.Error("non-Opaque Secret must drop")
	}

	// Create/Delete of an Opaque Secret pass.
	s := opaqueSecret("db-conn", map[string][]byte{"JDBC_PASSWORD": []byte("x")}, nil)
	if !p.Create(event.CreateEvent{Object: s}) {
		t.Error("Opaque Create must pass")
	}
	if !p.Delete(event.DeleteEvent{Object: s}) {
		t.Error("Opaque Delete must pass")
	}

	// Metadata-only edit (label added, same .data) → drop.
	oldS := opaqueSecret("db-conn", map[string][]byte{"JDBC_PASSWORD": []byte("x")}, nil)
	labeled := opaqueSecret("db-conn", map[string][]byte{"JDBC_PASSWORD": []byte("x")}, map[string]string{"team": "iam"})
	if p.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: labeled}) {
		t.Error("metadata-only edit must drop (no .data change)")
	}

	// .data change → pass.
	changed := opaqueSecret("db-conn", map[string][]byte{"JDBC_PASSWORD": []byte("fixed")}, nil)
	if !p.Update(event.UpdateEvent{ObjectOld: oldS, ObjectNew: changed}) {
		t.Error(".data change must pass")
	}
}

func TestJobStatusDiffers(t *testing.T) {
	base := &batchv1.Job{Status: batchv1.JobStatus{Active: 1}}

	if jobStatusDiffers(base, base.DeepCopy()) {
		t.Error("identical status should not differ")
	}

	succeeded := base.DeepCopy()
	succeeded.Status.Active = 0
	succeeded.Status.Succeeded = 1
	if !jobStatusDiffers(base, succeeded) {
		t.Error("succeeded-count change should differ")
	}

	failed := base.DeepCopy()
	failed.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if !jobStatusDiffers(base, failed) {
		t.Error("Failed condition appearing should differ")
	}

	readyOnly := base.DeepCopy()
	readyOnly.Status.Ready = ptr.To(int32(1))
	if !jobStatusDiffers(base, readyOnly) {
		t.Error("ready-count change should differ")
	}
}

func TestJobStatusChangedPredicate_Update(t *testing.T) {
	p := jobStatusChangedPredicate{}
	old := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"a": "1"}}}

	labelOnly := old.DeepCopy()
	labelOnly.Labels["a"] = "2"
	if p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: labelOnly}) {
		t.Error("label-only update should be dropped")
	}

	statusChange := old.DeepCopy()
	statusChange.Status.Succeeded = 1
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: statusChange}) {
		t.Error("status change should pass")
	}

	deleting := old.DeepCopy()
	now := metav1.Now()
	deleting.DeletionTimestamp = &now
	if !p.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: deleting}) {
		t.Error("deletionTimestamp set should pass")
	}

	if p.Update(event.UpdateEvent{ObjectOld: &corev1.Pod{}, ObjectNew: &corev1.Pod{}}) {
		t.Error("non-Job update should be dropped")
	}
}
