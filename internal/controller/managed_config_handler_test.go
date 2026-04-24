package controller

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// mapByName returns a managedConfigMapFunc that looks up each object's Name in
// a map and returns the associated []ctrl.Request. This lets each test declare
// exactly which clusters an object should enqueue, independent of the real
// scope-matching logic.
func mapByName(routing map[string][]ctrl.Request) managedConfigMapFunc {
	return func(_ context.Context, obj client.Object) []ctrl.Request {
		if obj == nil {
			return nil
		}
		return routing[obj.GetName()]
	}
}

func newTestQueue() *controllertest.Queue {
	return &controllertest.Queue{TypedInterface: workqueue.NewTyped[reconcile.Request]()}
}

func drainQueue(q workqueue.TypedRateLimitingInterface[reconcile.Request]) []ctrl.Request {
	out := make([]ctrl.Request, 0, q.Len())
	for q.Len() > 0 {
		item, shutdown := q.Get()
		if shutdown {
			break
		}
		out = append(out, item)
		q.Done(item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func cm(name string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
}

func TestManagedConfigHandler_UpdateUnionsOldAndNew_ClusterLeavingScope(t *testing.T) {
	// Regression guard: when a ConfigMap moves from cluster-a scope to no scope
	// (annotation removed or changed), the old mapping returns [cluster-a] but
	// the new mapping returns nothing. Without the union the cluster would
	// silently stop reconciling on this change.
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"old": {req("ns", "cluster-a")},
		"new": nil,
	}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("leaving-scope: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_UpdateUnionsOldAndNew_ClusterEnteringScope(t *testing.T) {
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"old": nil,
		"new": {req("ns", "cluster-a")},
	}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("entering-scope: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_UpdateUnionsOldAndNew_DisjointSets(t *testing.T) {
	// Both "moved from" and "moved to" clusters must reconcile in one cycle.
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"old": {req("ns", "cluster-a")},
		"new": {req("ns", "cluster-b")},
	}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a"), req("ns", "cluster-b")}
	if !equalRequests(got, want) {
		t.Errorf("disjoint: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_UpdateUnionsOldAndNew_OverlappingSets(t *testing.T) {
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"old": {req("ns", "cluster-a"), req("ns", "cluster-b")},
		"new": {req("ns", "cluster-b"), req("ns", "cluster-c")},
	}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{
		req("ns", "cluster-a"),
		req("ns", "cluster-b"),
		req("ns", "cluster-c"),
	}
	if !equalRequests(got, want) {
		t.Errorf("overlapping: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_UpdateDeduplicatesIdenticalSets(t *testing.T) {
	// workqueue.Add already deduplicates, but verify we don't rely on that
	// accidentally — the handler itself must union via a set so the no-op
	// case behaves correctly even if the queue semantics ever change.
	reqs := []ctrl.Request{req("ns", "cluster-a")}
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"old": reqs,
		"new": reqs,
	}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("dedup: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_CreateEnqueuesMappedRequests(t *testing.T) {
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"obj": {req("ns", "cluster-a"), req("ns", "cluster-b")},
	}))
	q := newTestQueue()
	h.Create(context.Background(), event.CreateEvent{Object: cm("obj")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a"), req("ns", "cluster-b")}
	if !equalRequests(got, want) {
		t.Errorf("create: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_DeleteEnqueuesMappedRequests(t *testing.T) {
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"obj": {req("ns", "cluster-a")},
	}))
	q := newTestQueue()
	h.Delete(context.Background(), event.DeleteEvent{Object: cm("obj")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("delete: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_GenericEnqueuesMappedRequests(t *testing.T) {
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"obj": {req("ns", "cluster-a")},
	}))
	q := newTestQueue()
	h.Generic(context.Background(), event.GenericEvent{Object: cm("obj")}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("generic: got %v, want %v", got, want)
	}
}

func TestManagedConfigHandler_UpdateEmptyBoth(t *testing.T) {
	// Neither object maps to any cluster (e.g. label/annotation missing) —
	// nothing should be enqueued.
	h := newManagedConfigHandler(mapByName(map[string][]ctrl.Request{}))
	q := newTestQueue()
	h.Update(context.Background(), event.UpdateEvent{ObjectOld: cm("old"), ObjectNew: cm("new")}, q)

	if q.Len() != 0 {
		t.Errorf("expected empty queue, got %d items", q.Len())
	}
}

func equalRequests(a, b []ctrl.Request) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
