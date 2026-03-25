package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func TestComputeNodeConditions_AllReady(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3, AvailableReplicas: 3, UpdatedReplicas: 3},
	}

	conditions, rs := computeNodeConditions(deploy, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	assertCondition(t, conditions, v1alpha1.ConditionAvailable, metav1.ConditionTrue)
	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse)

	if rs.Replicas != 3 {
		t.Errorf("expected replicas 3, got %d", rs.Replicas)
	}
}

func TestComputeNodeConditions_ZeroReady(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0, AvailableReplicas: 0, UpdatedReplicas: 2},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse) // not degraded, just unavailable
}

func TestComputeNodeConditions_PartialReady(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 3},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	// Available=False because 1/3 < desired (minimum availability not met)
	assertCondition(t, conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)
}

func TestComputeNodeConditions_NilDeployment(t *testing.T) {
	conditions, _ := computeNodeConditions(nil, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue)
}

func TestComputeNodeConditions_RolloutInProgress(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3, AvailableReplicas: 3, UpdatedReplicas: 1},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue)
}

func TestComputeNodeConditions_StuckRollout(t *testing.T) {
	// Simulates a stuck rolling update: old pod running (ready=1, available=1),
	// new pod crashing (unavailable=1), but updatedReplicas==desired.
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:       1,
			AvailableReplicas:   1,
			UpdatedReplicas:     1,
			UnavailableReplicas: 1,
		},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	// Should detect stuck rollout via unavailableReplicas > 0
	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue)

	// Verify the reason is ReplicaUnavailable, not RolloutInProgress
	for _, c := range conditions {
		if c.Type == v1alpha1.ConditionProgressing {
			if c.Reason != "ReplicaUnavailable" {
				t.Errorf("expected reason ReplicaUnavailable, got %s", c.Reason)
			}
		}
	}
}

func TestComputeNodeConditions_RolloutComplete(t *testing.T) {
	// Fully settled: all updated, all ready, none unavailable
	deploy := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas:       2,
			AvailableReplicas:   2,
			UpdatedReplicas:     2,
			UnavailableReplicas: 0,
		},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionFalse)
}

func TestComputeNodeConditions_ObservedGeneration(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1, AvailableReplicas: 1, UpdatedReplicas: 1},
	}

	conditions, _ := computeNodeConditions(deploy, 42)

	for _, c := range conditions {
		if c.ObservedGeneration != 42 {
			t.Errorf("condition %s: expected observedGeneration 42, got %d", c.Type, c.ObservedGeneration)
		}
	}
}

func TestComputeClusterConditions_AllNodesReady(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeAdmin}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: readyConditions(),
		}},
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: readyConditions(),
		}},
	}

	conditions := computeClusterConditions(nodes, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionTrue)
	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse)
}

func TestComputeClusterConditions_OneNodeNotReady(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: readyConditions(),
		}},
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: notReadyConditions(),
		}},
	}

	conditions := computeClusterConditions(nodes, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
}

func TestComputeClusterConditions_NoNodes(t *testing.T) {
	conditions := computeClusterConditions(nil, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionAvailable, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionProgressing, metav1.ConditionTrue)
}

func TestComputeClusterConditions_TwoAdminNodes(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeAdmin}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: readyConditions(),
		}},
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeAdmin}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: readyConditions(),
		}},
	}

	conditions := computeClusterConditions(nodes, 1)

	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)
}

func TestComputeClusterConditions_DegradedNode(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: degradedConditions(),
		}},
	}

	conditions := computeClusterConditions(nodes, 1)

	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionTrue)
}

// --- helpers ---

func readyConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionTrue},
		{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue},
		{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionFalse},
	}
}

func notReadyConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse},
		{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionFalse},
	}
}

func degradedConditions() []metav1.Condition {
	return []metav1.Condition{
		{Type: v1alpha1.ConditionReady, Status: metav1.ConditionFalse},
		{Type: v1alpha1.ConditionAvailable, Status: metav1.ConditionTrue},
		{Type: v1alpha1.ConditionDegraded, Status: metav1.ConditionTrue},
	}
}

func assertCondition(t *testing.T, conditions []metav1.Condition, condType string, expectedStatus metav1.ConditionStatus) {
	t.Helper()
	cond := apimeta.FindStatusCondition(conditions, condType)
	if cond == nil {
		t.Errorf("condition %s not found", condType)
		return
	}
	if cond.Status != expectedStatus {
		t.Errorf("condition %s: expected %s, got %s (reason: %s)", condType, expectedStatus, cond.Status, cond.Reason)
	}
}
