package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

func clusterFixture(name string, uid types.UID) *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: uid},
	}
}

func canonicalRef(name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               "IdentityServerCluster",
		Name:               name,
		UID:                uid,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

func TestEnsureClusterOwnerRef_AppendsWhenAbsent(t *testing.T) {
	node := &v1alpha1.IdentityServerNode{}
	cluster := clusterFixture("c1", "uid-1")

	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("expected mutation on empty ref slice")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(node.OwnerReferences))
	}
	got := node.OwnerReferences[0]
	want := canonicalRef("c1", "uid-1")
	if got.UID != want.UID || got.Name != want.Name || got.Kind != want.Kind {
		t.Errorf("ref mismatch: got %+v want %+v", got, want)
	}
	if got.Controller == nil || !*got.Controller {
		t.Error("expected Controller=true")
	}
	if got.BlockOwnerDeletion == nil || !*got.BlockOwnerDeletion {
		t.Error("expected BlockOwnerDeletion=true")
	}
}

func TestEnsureClusterOwnerRef_IdempotentWhenCorrect(t *testing.T) {
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{canonicalRef("c1", "uid-1")},
		},
	}
	if ensureClusterOwnerRef(node, clusterFixture("c1", "uid-1")) {
		t.Fatal("expected no mutation when ref already correct")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("slice should be untouched, got len %d", len(node.OwnerReferences))
	}
}

func TestEnsureClusterOwnerRef_RoundTripIdempotent(t *testing.T) {
	// Regression guard against a reconcile loop: feed the OUTPUT of one call
	// back into a second call. If the second call returns true, the reconciler
	// would emit r.Update on every pass and loop forever. The
	// _IdempotentWhenCorrect test above uses a hand-built canonicalRef fixture
	// — it does not catch drift between that fixture and the ref that
	// ensureClusterOwnerRef actually constructs in production.
	node := &v1alpha1.IdentityServerNode{}
	cluster := clusterFixture("c1", "uid-1")

	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("first call: expected mutation on empty ref slice")
	}
	if ensureClusterOwnerRef(node, cluster) {
		t.Fatal("second call: expected no-op on an already-canonical ref " +
			"(production-built ref must round-trip cleanly)")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("expected exactly 1 ref after round-trip, got %d", len(node.OwnerReferences))
	}
}

func TestEnsureClusterOwnerRef_NilControllerPointerIsStale(t *testing.T) {
	bad := canonicalRef("c1", "uid-1")
	bad.Controller = nil
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{bad}},
	}
	if !ensureClusterOwnerRef(node, clusterFixture("c1", "uid-1")) {
		t.Fatal("expected mutation: nil Controller must be replaced")
	}
	if got := node.OwnerReferences[0].Controller; got == nil || !*got {
		t.Errorf("Controller not fixed: %v", got)
	}
}

func TestEnsureClusterOwnerRef_NilBlockOwnerDeletionPointerIsStale(t *testing.T) {
	bad := canonicalRef("c1", "uid-1")
	bad.BlockOwnerDeletion = nil
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{bad}},
	}
	if !ensureClusterOwnerRef(node, clusterFixture("c1", "uid-1")) {
		t.Fatal("expected mutation: nil BlockOwnerDeletion must be replaced")
	}
	if got := node.OwnerReferences[0].BlockOwnerDeletion; got == nil || !*got {
		t.Errorf("BlockOwnerDeletion not fixed: %v", got)
	}
}

func TestEnsureClusterOwnerRef_StaleUIDReplaced(t *testing.T) {
	stale := canonicalRef("c1", "uid-OLD")
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{stale}},
	}
	cluster := clusterFixture("c1", "uid-NEW")
	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("expected mutation on stale UID")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(node.OwnerReferences))
	}
	if node.OwnerReferences[0].UID != "uid-NEW" {
		t.Errorf("UID not updated: got %q", node.OwnerReferences[0].UID)
	}
}

func TestEnsureClusterOwnerRef_DuplicateStaleRefRemoved(t *testing.T) {
	// Two IdentityServerCluster-kind refs: one stale, one correct.
	stale := canonicalRef("c1", "uid-OLD")
	correct := canonicalRef("c1", "uid-NEW")
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{stale, correct},
		},
	}
	cluster := clusterFixture("c1", "uid-NEW")
	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("expected mutation: stale duplicate should be dropped")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ref after dedupe, got %d: %+v", len(node.OwnerReferences), node.OwnerReferences)
	}
	if node.OwnerReferences[0].UID != "uid-NEW" {
		t.Errorf("kept wrong ref: %+v", node.OwnerReferences[0])
	}
}

func TestEnsureClusterOwnerRef_PreservesUnrelatedRefs(t *testing.T) {
	other := metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "rs-abc",
		UID:        "uid-rs",
		Controller: ptr.To(true),
	}
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{other}},
	}
	cluster := clusterFixture("c1", "uid-1")
	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("expected mutation")
	}
	if len(node.OwnerReferences) != 2 {
		t.Fatalf("expected 2 refs (unrelated + cluster), got %d", len(node.OwnerReferences))
	}
	var foundOther, foundCluster bool
	for _, r := range node.OwnerReferences {
		if r.Kind == "ReplicaSet" && r.UID == "uid-rs" {
			foundOther = true
		}
		if r.Kind == "IdentityServerCluster" && r.UID == "uid-1" {
			foundCluster = true
		}
	}
	if !foundOther {
		t.Error("unrelated ReplicaSet owner ref was dropped")
	}
	if !foundCluster {
		t.Error("cluster owner ref not added")
	}
}

func TestEnsureClusterOwnerRef_StaleRefOnly_NoExactDupe(t *testing.T) {
	// Two stale IdentityServerCluster refs (both wrong UID) — both should be
	// removed and a single canonical ref added.
	stale1 := canonicalRef("c1", "uid-A")
	stale2 := canonicalRef("c1", "uid-B")
	node := &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{stale1, stale2},
		},
	}
	cluster := clusterFixture("c1", "uid-NEW")
	if !ensureClusterOwnerRef(node, cluster) {
		t.Fatal("expected mutation")
	}
	if len(node.OwnerReferences) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(node.OwnerReferences))
	}
	if node.OwnerReferences[0].UID != "uid-NEW" {
		t.Errorf("wrong UID kept: %q", node.OwnerReferences[0].UID)
	}
}
