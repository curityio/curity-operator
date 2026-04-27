package controller

import (
	"context"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newClusterTestScheme(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s)
}

func clusterObj(name, namespace string) *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func TestFindClustersForManagedConfig_NoManagedLabel_ReturnsNil(t *testing.T) {
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(clusterObj("cluster-a", "ns")).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "ns"},
	}
	if got := r.findClustersForManagedConfig(ctx, obj); got != nil {
		t.Errorf("expected nil for unmanaged object, got %v", got)
	}
}

func TestFindClustersForManagedConfig_NoAnnotation_AppliesToAll(t *testing.T) {
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(
		clusterObj("cluster-a", "ns"),
		clusterObj("cluster-b", "ns"),
	).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", nil)

	got := r.findClustersForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-b"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("no annotation: got %v, want %v", got, want)
	}
}

func TestFindClustersForManagedConfig_SingleScope_FiltersByName(t *testing.T) {
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(
		clusterObj("cluster-a", "ns"),
		clusterObj("cluster-b", "ns"),
	).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "cluster-a",
	})

	got := r.findClustersForManagedConfig(ctx, obj)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-a"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("single scope: got %v, want %v", got, want)
	}
}

func TestFindClustersForManagedConfig_EmptyScope_FansOutToAll(t *testing.T) {
	// Empty scope matches no cluster — fan out so at least one reconciler
	// emits EmptyClusterScope on the CM. UID dedup makes the fan-out free.
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(
		clusterObj("cluster-a", "ns"),
		clusterObj("cluster-b", "ns"),
	).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "",
	})

	got := r.findClustersForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-b"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("empty scope fan-out: got %v, want %v", got, want)
	}
}

func TestFindClustersForManagedConfig_AllUnknownScope_FansOutToAll(t *testing.T) {
	// All-typo scope matches no cluster — fan out so at least one
	// reconciler emits UnknownClusterInScope on the CM.
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(
		clusterObj("cluster-a", "ns"),
		clusterObj("cluster-b", "ns"),
	).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "typo-a,typo-b",
	})

	got := r.findClustersForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-b"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("all-unknown fan-out: got %v, want %v", got, want)
	}
}

func TestFindClustersForManagedConfig_PartialUnknownScope_DoesNotFanOut(t *testing.T) {
	// When at least one listed name matches an existing cluster, the normal
	// loop enqueues that cluster and the reconcile itself picks up the
	// remaining unknowns in the scope scan. No fan-out needed.
	ctx := context.Background()
	c := newClusterTestScheme(t).WithObjects(
		clusterObj("cluster-a", "ns"),
		clusterObj("cluster-b", "ns"),
	).Build()
	r := &IdentityServerClusterReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "cluster-a,does-not-exist",
	})

	got := r.findClustersForManagedConfig(ctx, obj)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "cluster-a"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("partial-unknown: got %v, want %v", got, want)
	}
}
