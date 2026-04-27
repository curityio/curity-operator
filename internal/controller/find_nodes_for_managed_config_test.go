package controller

import (
	"context"
	"sort"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func managedConfigMap(name, namespace string, annotations map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: annotations,
		},
	}
}

func nodeWithCluster(name, namespace, clusterName string) *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.IdentityServerNodeSpec{
			IdentityServerClusterRef: v1alpha1.ObjectReference{Name: clusterName},
		},
	}
}

func newNodeTestScheme(t *testing.T) *fake.ClientBuilder {
	t.Helper()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s)
}

func sortRequests(reqs []ctrl.Request) {
	sort.Slice(reqs, func(i, j int) bool {
		if reqs[i].Namespace != reqs[j].Namespace {
			return reqs[i].Namespace < reqs[j].Namespace
		}
		return reqs[i].Name < reqs[j].Name
	})
}

func TestFindNodesForManagedConfig_NoManagedLabel_ReturnsNil(t *testing.T) {
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	// ConfigMap without the curity.io/managed=true label.
	obj := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unmanaged", Namespace: "ns"},
	}
	if got := r.findNodesForManagedConfig(ctx, obj); got != nil {
		t.Errorf("expected nil for unmanaged object, got %v", got)
	}
}

func TestFindNodesForManagedConfig_NoAnnotation_AppliesToAll(t *testing.T) {
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", nil)

	got := r.findNodesForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-b"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("no annotation: got %v, want %v", got, want)
	}
}

func TestFindNodesForManagedConfig_EmptyAnnotation_AppliesToNone(t *testing.T) {
	// An empty curity.io/cluster value means "applies to no cluster" (user
	// mistake) rather than "applies to all". The map-func must enqueue no
	// nodes; the cluster reconciler separately surfaces the resource via
	// the EmptyClusterScope Warning Event emitted on the offending CM/Secret.
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "",
	})

	got := r.findNodesForManagedConfig(ctx, obj)
	if len(got) != 0 {
		t.Errorf("empty annotation must enqueue no nodes, got %v", got)
	}
}

func TestFindNodesForManagedConfig_SingleScope_FiltersByClusterRef(t *testing.T) {
	// Only nodes whose IdentityServerClusterRef.Name is in the scope should
	// be enqueued. This is the feature's main code path.
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a1", "ns", "cluster-a"),
		nodeWithCluster("node-a2", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "cluster-a",
	})

	got := r.findNodesForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-a1"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-a2"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("single scope: got %v, want %v", got, want)
	}
}

func TestFindNodesForManagedConfig_MultiScope_FiltersByClusterRef(t *testing.T) {
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
		nodeWithCluster("node-c", "ns", "cluster-c"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "cluster-a, cluster-c",
	})

	got := r.findNodesForManagedConfig(ctx, obj)
	sortRequests(got)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-a"}},
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-c"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("multi scope: got %v, want %v", got, want)
	}
}

func TestFindNodesForManagedConfig_NoMatch_ReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns", map[string]string{
		AnnotationClusterScope: "cluster-missing",
	})

	got := r.findNodesForManagedConfig(ctx, obj)
	if len(got) != 0 {
		t.Errorf("expected no requests, got %v", got)
	}
}

func TestFindNodesForManagedConfig_IsNamespaceScoped(t *testing.T) {
	// Nodes in other namespaces must not be enqueued — managed config events
	// only fan out to nodes in the ConfigMap/Secret's own namespace.
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns-1", "cluster-a"),
		nodeWithCluster("node-other", "ns-2", "cluster-a"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := managedConfigMap("cm", "ns-1", map[string]string{
		AnnotationClusterScope: "cluster-a",
	})

	got := r.findNodesForManagedConfig(ctx, obj)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns-1", Name: "node-a"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("namespace scoping: got %v, want %v", got, want)
	}
}

func TestFindNodesForManagedConfig_Secret_FiltersByClusterRef(t *testing.T) {
	// Secrets route through the same map-func; verify a non-ConfigMap managed
	// object also gets filtered correctly.
	ctx := context.Background()
	c := newNodeTestScheme(t).WithObjects(
		nodeWithCluster("node-a", "ns", "cluster-a"),
		nodeWithCluster("node-b", "ns", "cluster-b"),
	).Build()
	r := &IdentityServerNodeReconciler{Client: c}

	obj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "secret",
			Namespace:   "ns",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: map[string]string{AnnotationClusterScope: "cluster-b"},
		},
	}

	got := r.findNodesForManagedConfig(ctx, obj)
	want := []ctrl.Request{
		{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "node-b"}},
	}
	if !equalRequests(got, want) {
		t.Errorf("secret filter: got %v, want %v", got, want)
	}
}
