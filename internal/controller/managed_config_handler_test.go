package controller

import (
	"context"
	"sort"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// mapByName returns a scopeMapFunc that looks up each object's Name in
// a map and returns the associated []ctrl.Request. This lets each test declare
// exactly which clusters an object should enqueue, independent of the real
// scope-matching logic.
func mapByName(routing map[string][]ctrl.Request) scopeMapFunc {
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{
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
	h := newUnionScopeHandler(mapByName(map[string][]ctrl.Request{}))
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

type recordingSink struct {
	infos []logEntry
}

type logEntry struct {
	msg    string
	fields map[string]any
}

func (s *recordingSink) Init(logr.RuntimeInfo)          {}
func (s *recordingSink) Enabled(int) bool               { return true }
func (s *recordingSink) Error(error, string, ...any)    {}
func (s *recordingSink) WithName(string) logr.LogSink   { return s }
func (s *recordingSink) WithValues(...any) logr.LogSink { return s }
func (s *recordingSink) Info(_ int, msg string, kv ...any) {
	fields := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		k, _ := kv[i].(string)
		fields[k] = kv[i+1]
	}
	s.infos = append(s.infos, logEntry{msg: msg, fields: fields})
}

func ctxWithSink() (context.Context, *recordingSink) {
	sink := &recordingSink{}
	return logf.IntoContext(context.Background(), logr.New(sink)), sink
}

func countLogs(entries []logEntry, msg string) int {
	n := 0
	for _, e := range entries {
		if e.msg == msg {
			n++
		}
	}
	return n
}

func findLog(entries []logEntry, msg string) logEntry {
	for _, e := range entries {
		if e.msg == msg {
			return e
		}
	}
	return logEntry{}
}

func handlerTestCM(name string, annotations map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "ns",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: annotations,
		},
	}
}

func handlerTestSecret(name string, annotations map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "ns",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: annotations,
		},
	}
}

func TestClusterManagedConfigHandler_LogsOnCreateOfUnannotatedCM(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{Object: handlerTestCM("cm-bare", nil)}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 1 {
		t.Fatalf("expected 1 log on Create, got %d (entries=%+v)", got, sink.infos)
	}
	e := findLog(sink.infos, implicitConfigTypeLogMsg)
	if e.fields["kind"] != "ConfigMap" || e.fields["name"] != "cm-bare" || e.fields["namespace"] != "ns" {
		t.Errorf("unexpected log fields: %+v", e.fields)
	}
}

func TestClusterManagedConfigHandler_LogsOnCreateOfUnannotatedSecret(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{Object: handlerTestSecret("sec-bare", nil)}, newTestQueue())

	e := findLog(sink.infos, implicitConfigTypeLogMsg)
	if e.fields["kind"] != "Secret" {
		t.Errorf("expected kind=Secret in log fields, got %+v", e.fields)
	}
}

func TestClusterManagedConfigHandler_NoLog_WhenAnnotationExplicitlyBase(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{
		Object: handlerTestCM("cm-base", map[string]string{AnnotationConfigType: ConfigTypeBase}),
	}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("expected zero logs when annotation set explicitly, got %d", got)
	}
}

func TestClusterManagedConfigHandler_NoLog_WhenAnnotationUnknownValue(t *testing.T) {
	// Any non-empty annotation value suppresses the log; UnknownConfigType
	// is surfaced separately by discovery.
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{
		Object: handlerTestCM("cm-bogus", map[string]string{AnnotationConfigType: "bogus"}),
	}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("expected zero logs for unknown annotation value, got %d", got)
	}
}

func TestClusterManagedConfigHandler_NoLog_OnDelete(t *testing.T) {
	// Resource is being removed — logging "treating as base" would mislead.
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Delete(ctx, event.DeleteEvent{Object: handlerTestCM("cm-bare", nil)}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("expected zero logs on Delete, got %d", got)
	}
}

func TestClusterManagedConfigHandler_LogsOnceOnUpdate_NewSideOnly(t *testing.T) {
	// Inner handler invokes the mapper twice on Update (old + new union);
	// the wrapper must not log twice.
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	old := handlerTestCM("cm-bare", nil)
	new := handlerTestCM("cm-bare", nil)
	h.Update(ctx, event.UpdateEvent{ObjectOld: old, ObjectNew: new}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 1 {
		t.Fatalf("expected exactly 1 log per Update event (new-side only), got %d (entries=%+v)", got, sink.infos)
	}
}

func TestClusterManagedConfigHandler_NoLog_OnUpdateWhenNewSideHasAnnotation(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	old := handlerTestCM("cm", nil)
	new := handlerTestCM("cm", map[string]string{AnnotationConfigType: ConfigTypeBase})
	h.Update(ctx, event.UpdateEvent{ObjectOld: old, ObjectNew: new}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("expected zero logs when new-side annotation is set, got %d", got)
	}
}

func TestClusterManagedConfigHandler_NoLog_OnUpdateLabelRemoval(t *testing.T) {
	// Predicate lets the Update through because old had the label; the new
	// side is no longer managed, so the log must not fire.
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	old := handlerTestCM("cm", nil)
	new := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cm", Namespace: "ns"}}
	h.Update(ctx, event.UpdateEvent{ObjectOld: old, ObjectNew: new}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("expected zero logs on label-removal Update, got %d", got)
	}
}

func TestClusterManagedConfigHandler_PerEventCount_TwoEventsTwoLogs(t *testing.T) {
	// Locks event-driven semantics against any future per-UID memoization.
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	obj := handlerTestCM("cm-bare", nil)
	h.Create(ctx, event.CreateEvent{Object: obj}, newTestQueue())
	h.Create(ctx, event.CreateEvent{Object: obj}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 2 {
		t.Errorf("expected 2 logs (one per event), got %d", got)
	}
}

func TestClusterManagedConfigHandler_NoLog_WhenUnlabeled(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{
		Object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled", Namespace: "ns"}},
	}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 0 {
		t.Errorf("unlabeled CM must not trigger the log, got %d", got)
	}
}

func TestClusterManagedConfigHandler_LogsOnCreateWithNilAnnotationsMap(t *testing.T) {
	ctx, sink := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(nil))

	h.Create(ctx, event.CreateEvent{Object: handlerTestCM("cm-nil", nil)}, newTestQueue())

	if got := countLogs(sink.infos, implicitConfigTypeLogMsg); got != 1 {
		t.Errorf("nil annotations should still log once, got %d", got)
	}
}

func TestClusterManagedConfigHandler_DelegatesRequestMapping(t *testing.T) {
	// Adding the log side-effect must not break cluster-scope dispatch.
	ctx, _ := ctxWithSink()
	h := newClusterManagedConfigHandler(mapByName(map[string][]ctrl.Request{
		"cm-bare": {req("ns", "cluster-a")},
	}))
	q := newTestQueue()

	h.Create(ctx, event.CreateEvent{Object: handlerTestCM("cm-bare", nil)}, q)

	got := drainQueue(q)
	want := []ctrl.Request{req("ns", "cluster-a")}
	if !equalRequests(got, want) {
		t.Errorf("delegation broke: got %v want %v", got, want)
	}
}
