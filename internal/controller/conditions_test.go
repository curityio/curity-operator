package controller

import (
	"strings"
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

// Regression: the Degraded=False branch at node level must not claim "Healthy"
// when zero replicas are ready. The condition value is still False (we are not
// in the partial-availability "degraded" state), but the reason/message must
// not assert overall health — Ready and Available already signal that.
func TestComputeNodeConditions_ZeroReady_NotClaimingHealthy(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(2))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 0, AvailableReplicas: 0, UpdatedReplicas: 2},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	degraded := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionDegraded)
	if degraded == nil {
		t.Fatal("Degraded condition not set")
	}
	if degraded.Status != metav1.ConditionFalse {
		t.Errorf("Degraded status = %s, want False", degraded.Status)
	}
	if degraded.Reason == "Healthy" {
		t.Errorf("Degraded reason must not claim 'Healthy' when 0/%d replicas are ready", 2)
	}
}

// Locks in the chosen reason name on the Degraded=False branch so a future
// drive-by rename trips this test.
func TestComputeNodeConditions_AllReady_ReasonNotDegraded(t *testing.T) {
	deploy := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3, AvailableReplicas: 3, UpdatedReplicas: 3},
	}

	conditions, _ := computeNodeConditions(deploy, 1)

	degraded := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionDegraded)
	if degraded == nil {
		t.Fatal("Degraded condition not set")
	}
	if degraded.Reason != "NotDegraded" {
		t.Errorf("Degraded reason = %q, want %q", degraded.Reason, "NotDegraded")
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

// Regression for the user-reported bug: during bootstrap (nodes exist but none
// Ready) the cluster-level Degraded=False branch must not announce "Healthy".
// Ready=False / Available=False / Progressing=True already signal that the
// cluster is not yet up; Degraded only knows about partial-availability.
func TestComputeClusterConditions_BootstrapDoesNotClaimHealthy(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeAdmin}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: notReadyConditions(),
		}},
		{Spec: v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime}, Status: v1alpha1.IdentityServerNodeStatus{
			Conditions: notReadyConditions(),
		}},
	}

	conditions := computeClusterConditions(nodes, 1)

	assertCondition(t, conditions, v1alpha1.ConditionReady, metav1.ConditionFalse)
	assertCondition(t, conditions, v1alpha1.ConditionDegraded, metav1.ConditionFalse)

	degraded := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionDegraded)
	if degraded == nil {
		t.Fatal("Degraded condition not set")
	}
	if degraded.Reason == "Healthy" {
		t.Errorf("Degraded reason must not claim 'Healthy' while no nodes are Ready")
	}
}

// =========================================================================
// PackagesReady cluster aggregation tests
// =========================================================================

// packagesReadyTrue builds a node-status conditions slice containing a
// PackagesReady=True condition (plus the standard Ready/Available stubs so
// the rest of computeClusterConditions has values to roll up).
func packagesReadyTrue() []metav1.Condition {
	return append(readyConditions(),
		metav1.Condition{
			Type:   v1alpha1.ConditionPackagesReady,
			Status: metav1.ConditionTrue,
			Reason: v1alpha1.ReasonAllPackagesFetched,
		},
	)
}

// packagesReadyFalse builds a node-status conditions slice containing a
// PackagesReady=False condition with the given reason and message.
func packagesReadyFalse(reason, message string) []metav1.Condition {
	return append(readyConditions(),
		metav1.Condition{
			Type:    v1alpha1.ConditionPackagesReady,
			Status:  metav1.ConditionFalse,
			Reason:  reason,
			Message: message,
		},
	)
}

func nodeNamed(name string, conditions []metav1.Condition) v1alpha1.IdentityServerNode {
	return v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.IdentityServerNodeSpec{Type: v1alpha1.NodeTypeRuntime},
		Status:     v1alpha1.IdentityServerNodeStatus{Conditions: conditions},
	}
}

// No child node carries PackagesReady → the cluster-level condition must be
// absent entirely (plan F5).
func TestComputeClusterConditions_PackagesReadyMirroring_AllNodesAbsent(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("a", readyConditions()),
		nodeNamed("b", readyConditions()),
	}
	conditions := computeClusterConditions(nodes, 1)
	if apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady) != nil {
		t.Error("PackagesReady must be omitted when no node has it set")
	}
}

// Every node-with-condition is True → cluster is True with success count.
func TestComputeClusterConditions_PackagesReadyMirroring_AllTrue(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("a", packagesReadyTrue()),
		nodeNamed("b", packagesReadyTrue()),
	}
	conditions := computeClusterConditions(nodes, 1)
	got := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady)
	if got == nil {
		t.Fatal("PackagesReady must be set when all nodes have it True")
	}
	if got.Status != metav1.ConditionTrue || got.Reason != v1alpha1.ReasonAllPackagesFetched {
		t.Errorf("got %+v, want True/AllPackagesFetched", got)
	}
	if !strings.Contains(got.Message, "2 node") {
		t.Errorf("multi-node True message must include node count: %q", got.Message)
	}
}

// Mixed: one node True, others absent → still True (count reflects nodes that
// have the condition set).
func TestComputeClusterConditions_PackagesReadyMirroring_TrueWithAbsentPeers(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("a", readyConditions()),   // no PackagesReady
		nodeNamed("b", packagesReadyTrue()), // True
	}
	conditions := computeClusterConditions(nodes, 1)
	got := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady)
	if got == nil || got.Status != metav1.ConditionTrue {
		t.Errorf("expected True with one True/one absent; got %+v", got)
	}
	if !strings.Contains(got.Message, "1 node") {
		t.Errorf("count must reflect nodes-with-condition, not nodes-total: %q", got.Message)
	}
}

// Single-node cluster with one False → message is the node's verbatim
// message, no "<node>:" prefix.
func TestComputeClusterConditions_PackagesReadyMirroring_SingleNodeFalse(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("rt", packagesReadyFalse(v1alpha1.ReasonPackageSecretMissing, `Secret "x" not found`)),
	}
	conditions := computeClusterConditions(nodes, 1)
	got := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady)
	if got == nil || got.Status != metav1.ConditionFalse {
		t.Fatalf("expected False; got %+v", got)
	}
	if got.Reason != v1alpha1.ReasonPackageSecretMissing {
		t.Errorf("reason = %q, want %q", got.Reason, v1alpha1.ReasonPackageSecretMissing)
	}
	if got.Message != `Secret "x" not found` {
		t.Errorf("single-node message must be verbatim (no prefix): %q", got.Message)
	}
}

// Multi-node cluster with exactly one False → message is prefixed by the
// failing node's name.
func TestComputeClusterConditions_PackagesReadyMirroring_MultiNodeOneFail(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("rt-1", packagesReadyTrue()),
		nodeNamed("rt-2", packagesReadyFalse(v1alpha1.ReasonPackageHTTPError, `exited 22 (HTTP 4xx/5xx)`)),
	}
	conditions := computeClusterConditions(nodes, 1)
	got := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady)
	if got == nil || got.Reason != v1alpha1.ReasonPackageHTTPError {
		t.Fatalf("expected HTTPError; got %+v", got)
	}
	if !strings.HasPrefix(got.Message, "rt-2: ") {
		t.Errorf("multi-node single-fail must prefix with node name: %q", got.Message)
	}
}

// Multi-node, multiple failures → message reports F/N count plus the first
// failing node (sorted by name).
func TestComputeClusterConditions_PackagesReadyMirroring_MultiNodeMultiFail(t *testing.T) {
	nodes := []v1alpha1.IdentityServerNode{
		nodeNamed("rt-z", packagesReadyFalse(v1alpha1.ReasonPackageHTTPError, "exit 22")),
		nodeNamed("rt-a", packagesReadyFalse(v1alpha1.ReasonPackageSecretMissing, `Secret "x" not found`)),
	}
	conditions := computeClusterConditions(nodes, 1)
	got := apimeta.FindStatusCondition(conditions, v1alpha1.ConditionPackagesReady)
	if got == nil {
		t.Fatal("expected set")
	}
	// Deterministic: sorted by name → "rt-a" is first → SecretMissing wins.
	if got.Reason != v1alpha1.ReasonPackageSecretMissing {
		t.Errorf("first-by-name reason expected; got %q", got.Reason)
	}
	if !strings.Contains(got.Message, "2/2 nodes") {
		t.Errorf("message must include failure count: %q", got.Message)
	}
	if !strings.Contains(got.Message, "first: rt-a:") {
		t.Errorf("message must name the first failing node: %q", got.Message)
	}
}

// Determinism: shuffling node order in the input must not change the chosen
// "first failing" node — sorting by name happens inside aggregatePackagesReady.
func TestComputeClusterConditions_PackagesReadyMirroring_FirstFailDeterministic(t *testing.T) {
	a := nodeNamed("a", packagesReadyFalse(v1alpha1.ReasonPackageSecretMissing, `Secret "a" not found`))
	z := nodeNamed("z", packagesReadyFalse(v1alpha1.ReasonPackageHTTPError, "exit 22"))

	for _, order := range [][]v1alpha1.IdentityServerNode{{a, z}, {z, a}} {
		conds := computeClusterConditions(order, 1)
		got := apimeta.FindStatusCondition(conds, v1alpha1.ConditionPackagesReady)
		if got == nil {
			t.Fatal("expected set")
		}
		if got.Reason != v1alpha1.ReasonPackageSecretMissing {
			t.Errorf("input order %v: expected SecretMissing (from node 'a'); got %q",
				[]string{order[0].Name, order[1].Name}, got.Reason)
		}
	}
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
