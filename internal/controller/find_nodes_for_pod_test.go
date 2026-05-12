package controller

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// curityPodForNode builds a Pod carrying the labels buildLabels stamps on the
// pod template plus a controller-ref pointing at a synthetic ReplicaSet. The
// `app.kubernetes.io/instance` value is computed via OwnedResourceName so it
// matches what findNodesForPod compares against.
func curityPodForNode(podName, namespace, clusterName, nodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "curity-operator",
				"app.kubernetes.io/instance":   OwnedResourceName(clusterName, nodeName),
				"curity.io/cluster":            clusterName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1",
				Kind:       "ReplicaSet",
				Name:       OwnedResourceName(clusterName, nodeName) + "-abcd",
				UID:        types.UID("rs-uid-" + podName),
				Controller: ptr.To(true),
			}},
		},
	}
}

// labeledNode constructs an IdentityServerNode pre-stamped with the
// curity.io/cluster label that the reconciler sets after the first reconcile.
// findNodesForPod relies on this label for its MatchingLabels list.
func labeledNode(name, namespace, clusterName string) *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"curity.io/cluster": clusterName},
		},
		Spec: v1alpha1.IdentityServerNodeSpec{
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
		},
	}
}

// reconcilerWithObjects builds a node reconciler backed by a fake client
// preloaded with the given objects.
func reconcilerWithObjects(t *testing.T, objs ...client.Object) *IdentityServerNodeReconciler {
	t.Helper()
	return &IdentityServerNodeReconciler{Client: newNodeTestScheme(t).WithObjects(objs...).Build()}
}

// Happy path: a Pod produced by a Deployment owned by node "rt-1" maps back
// to a reconcile request for that node.
func TestFindNodesForPod_HappyPath(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	node := labeledNode("rt-1", "ns", "tour")
	r := reconcilerWithObjects(t, node)

	reqs := r.findNodesForPod(context.Background(), pod)
	if len(reqs) != 1 {
		t.Fatalf("len(reqs) = %d, want 1; got %+v", len(reqs), reqs)
	}
	if reqs[0].Namespace != "ns" || reqs[0].Name != "rt-1" {
		t.Errorf("request = %+v, want {Namespace=ns, Name=rt-1}", reqs[0])
	}
}

// A non-Pod object on the watch path is silently dropped.
func TestFindNodesForPod_NonPodObject(t *testing.T) {
	r := reconcilerWithObjects(t)
	reqs := r.findNodesForPod(context.Background(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns"},
	})
	if reqs != nil {
		t.Errorf("non-Pod input must return nil; got %+v", reqs)
	}
}

// Pod missing the ReplicaSet controller-ref (hand-crafted / migrated workload)
// is dropped — the contract is pods produced by our Deployments only.
func TestFindNodesForPod_NoReplicaSetController(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	pod.OwnerReferences = nil
	node := labeledNode("rt-1", "ns", "tour")
	r := reconcilerWithObjects(t, node)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("pod without a controller ReplicaSet ref must be dropped; got %+v", reqs)
	}
}

// A ReplicaSet owner where Controller=nil (non-controlling reference) is dropped.
func TestFindNodesForPod_OwnerRefNotControlling(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	pod.OwnerReferences[0].Controller = nil
	node := labeledNode("rt-1", "ns", "tour")
	r := reconcilerWithObjects(t, node)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("non-controller OwnerRef must be dropped; got %+v", reqs)
	}
}

// Pod missing app.kubernetes.io/instance label is dropped.
func TestFindNodesForPod_MissingInstanceLabel(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	delete(pod.Labels, "app.kubernetes.io/instance")
	node := labeledNode("rt-1", "ns", "tour")
	r := reconcilerWithObjects(t, node)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("missing instance label must drop; got %+v", reqs)
	}
}

// Pod missing curity.io/cluster label is dropped.
func TestFindNodesForPod_MissingClusterLabel(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	delete(pod.Labels, "curity.io/cluster")
	node := labeledNode("rt-1", "ns", "tour")
	r := reconcilerWithObjects(t, node)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("missing curity.io/cluster label must drop; got %+v", reqs)
	}
}

// No ISN matches the pod's instance label (e.g. node was deleted just before
// the pod event fired). Returns empty result, no panic.
func TestFindNodesForPod_NoMatchingNode(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-deleted")
	otherNode := labeledNode("rt-other", "ns", "tour")
	r := reconcilerWithObjects(t, otherNode)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("no matching node must return nil; got %+v", reqs)
	}
}

// An ISN with the same name in a different cluster (same namespace) must not
// match — OwnedResourceName SHA-suffix ensures the comparison is cluster-aware.
func TestFindNodesForPod_DifferentClusterDoesNotMatch(t *testing.T) {
	// pod belongs to cluster "tour", node "rt-1"
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-1")
	// but only a node named "rt-1" in cluster "other" exists
	wrongClusterNode := labeledNode("rt-1", "ns", "other")
	r := reconcilerWithObjects(t, wrongClusterNode)

	reqs := r.findNodesForPod(context.Background(), pod)
	if reqs != nil {
		t.Errorf("ISN under a different cluster must not match; got %+v", reqs)
	}
}

// Two ISNs in the same namespace under the same cluster: the mapFunc must
// return the one whose OwnedResourceName matches the pod's instance label.
func TestFindNodesForPod_MultipleNodesPicksCorrectOne(t *testing.T) {
	pod := curityPodForNode("curity-pod-abc", "ns", "tour", "rt-2")
	rt1 := labeledNode("rt-1", "ns", "tour")
	rt2 := labeledNode("rt-2", "ns", "tour")
	r := reconcilerWithObjects(t, rt1, rt2)

	reqs := r.findNodesForPod(context.Background(), pod)
	if len(reqs) != 1 || reqs[0].Name != "rt-2" {
		t.Errorf("expected exactly one request for rt-2; got %+v", reqs)
	}
}

// hasReplicaSetController: a controlling ReplicaSet ref passes.
func TestHasReplicaSetController_ControllingReplicaSet(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Kind: "ReplicaSet", Controller: ptr.To(true)},
	}
	if !hasReplicaSetController(refs) {
		t.Error("controlling ReplicaSet ref must pass")
	}
}

// hasReplicaSetController: a non-controller ReplicaSet ref is dropped.
func TestHasReplicaSetController_NonControllerReplicaSet(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Kind: "ReplicaSet", Controller: ptr.To(false)},
	}
	if hasReplicaSetController(refs) {
		t.Error("non-controller ReplicaSet ref must be dropped")
	}
}

// hasReplicaSetController: a non-ReplicaSet controller ref is dropped.
func TestHasReplicaSetController_NonReplicaSetKind(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Kind: "StatefulSet", Controller: ptr.To(true)},
	}
	if hasReplicaSetController(refs) {
		t.Error("non-ReplicaSet kind must be dropped")
	}
}

// hasReplicaSetController: empty or nil OwnerReferences slice returns false.
func TestHasReplicaSetController_NoOwnerRefs(t *testing.T) {
	if hasReplicaSetController(nil) {
		t.Error("nil ownerRefs must return false")
	}
	if hasReplicaSetController([]metav1.OwnerReference{}) {
		t.Error("empty ownerRefs must return false")
	}
}

// hasReplicaSetController: multiple non-controller refs followed by a
// controller ReplicaSet ref must still pass — we iterate, not index [0].
// This is the defensive case from the plan's one remaining Unknown.
func TestHasReplicaSetController_ControllerNotFirst(t *testing.T) {
	refs := []metav1.OwnerReference{
		{Kind: "Deployment", Controller: ptr.To(false)}, // non-controller, not RS
		{Kind: "ReplicaSet", Controller: ptr.To(true)},  // the real owner
	}
	if !hasReplicaSetController(refs) {
		t.Error("must iterate ownerRefs — index [0] is not always the controlling ref")
	}
}

// deploymentReplicaFailureMessage helper tests.

// nil Deployment: defensive (a not-yet-existing deployment is healthy by
// definition — we have not stamped it yet).
func TestDeploymentReplicaFailureMessage_NilDeploy(t *testing.T) {
	msg, failing := deploymentReplicaFailureMessage(nil)
	if failing || msg != "" {
		t.Errorf("nil deploy must return ('', false); got (%q, %v)", msg, failing)
	}
}

// Deployment with no conditions yet (just-created) is treated as not
// failing — pod creation may simply be in flight.
func TestDeploymentReplicaFailureMessage_NoConditions(t *testing.T) {
	d := &appsv1.Deployment{}
	msg, failing := deploymentReplicaFailureMessage(d)
	if failing || msg != "" {
		t.Errorf("no conditions must return ('', false); got (%q, %v)", msg, failing)
	}
}

// ReplicaFailure=False (the steady-state healthy condition shape) must
// not be reported as failing.
func TestDeploymentReplicaFailureMessage_FalseCondition(t *testing.T) {
	d := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionFalse},
			},
		},
	}
	msg, failing := deploymentReplicaFailureMessage(d)
	if failing || msg != "" {
		t.Errorf("ReplicaFailure=False must return not-failing; got (%q, %v)", msg, failing)
	}
}

// ReplicaFailure=True with a verbatim K8s message (e.g., SCC rejection)
// must be surfaced unchanged so the user sees the actual diagnostic.
func TestDeploymentReplicaFailureMessage_TrueWithMessage(t *testing.T) {
	verbatim := `pod.metadata.annotations[container.seccomp.security.alpha.kubernetes.io/package-fetch-0]: Forbidden: seccomp may not be set`
	d := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: verbatim},
			},
		},
	}
	msg, failing := deploymentReplicaFailureMessage(d)
	if !failing {
		t.Fatal("ReplicaFailure=True must return failing=true")
	}
	if !strings.Contains(msg, verbatim) {
		t.Errorf("returned message must contain the verbatim K8s text: %q", msg)
	}
}

// Verbatim K8s messages can be huge — admission-webhook CEL traces hit
// 5-10 KiB easily. The helper must cap at replicaFailureMessageMaxLen
// to keep the condition compact, preserving the diagnostic head.
func TestDeploymentReplicaFailureMessage_TruncatesLongMessage(t *testing.T) {
	// Build a message well over the cap (8 KiB of "X" plus a
	// recognizable prefix at the start that must survive truncation).
	prefix := "ADMISSION_DENIED_KEY_ACTIONABLE_HEAD: "
	tail := strings.Repeat("X", 8*1024)
	verbatim := prefix + tail

	d := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: verbatim},
			},
		},
	}
	msg, failing := deploymentReplicaFailureMessage(d)
	if !failing {
		t.Fatal("ReplicaFailure=True must return failing=true")
	}
	if len(msg) > replicaFailureMessageMaxLen+512 {
		// allow some slack for the "Deployment ReplicaFailure: " prefix
		// and the "(truncated, N more bytes; ...)" suffix
		t.Errorf("message length %d exceeds cap+slack; truncation not applied: %q...", len(msg), msg[:120])
	}
	if !strings.Contains(msg, prefix) {
		t.Errorf("truncation must preserve the head of the diagnostic: got %q...", msg[:120])
	}
	if !strings.Contains(msg, "truncated") {
		t.Errorf("truncated message must include the (truncated, ...) indicator: %q", msg[len(msg)-200:])
	}
}

// ReplicaFailure=True with no message: fall back to the reason field so
// the condition still carries diagnostic information.
func TestDeploymentReplicaFailureMessage_TrueNoMessage(t *testing.T) {
	d := &appsv1.Deployment{
		Status: appsv1.DeploymentStatus{
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate"},
			},
		},
	}
	msg, failing := deploymentReplicaFailureMessage(d)
	if !failing {
		t.Fatal("ReplicaFailure=True must return failing=true even without Message")
	}
	if !strings.Contains(msg, "FailedCreate") {
		t.Errorf("fallback message must include the reason: %q", msg)
	}
}
