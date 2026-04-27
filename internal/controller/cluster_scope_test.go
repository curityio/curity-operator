package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// capturingRecorder records every Event call, preserving the involvedObject
// reference so tests can assert events land on the offending CM/Secret rather
// than on the cluster CR. The stdlib FakeRecorder loses the object pointer,
// so this lightweight wrapper is the simplest way to verify routing.
//
// Not goroutine-safe: one recorder per test. If a test is ever made parallel
// or spawns goroutines that emit events, add a sync.Mutex around events.
type capturingRecorder struct {
	events []capturedEvent
}

type capturedEvent struct {
	object    runtime.Object
	eventtype string
	reason    string
	message   string
}

func (r *capturingRecorder) Event(object runtime.Object, eventtype, reason, message string) {
	r.events = append(r.events, capturedEvent{object, eventtype, reason, message})
}

func (r *capturingRecorder) Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Event(object, eventtype, reason, fmt.Sprintf(messageFmt, args...))
}

func (r *capturingRecorder) AnnotatedEventf(object runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	r.Eventf(object, eventtype, reason, messageFmt, args...)
}

func setOf(names ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(names))
	for _, n := range names {
		out[n] = struct{}{}
	}
	return out
}

func TestParseClusterScope(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantNames map[string]struct{}
		wantEmpty bool
	}{
		{"empty_string", "", nil, true},
		{"whitespace_only", "   ", nil, true},
		{"single", "a", setOf("a"), false},
		{"multi", "a,b", setOf("a", "b"), false},
		{"trim", " a , b ", setOf("a", "b"), false},
		{"empty_entries_skipped", "a,,b", setOf("a", "b"), false},
		{"dedupe", "a,a,b", setOf("a", "b"), false},
		{"all_empty_entries", ",,", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, empty := parseClusterScope(tc.input)
			if empty != tc.wantEmpty {
				t.Errorf("empty: want %v, got %v", tc.wantEmpty, empty)
			}
			if !tc.wantEmpty && !reflect.DeepEqual(got, tc.wantNames) {
				t.Errorf("names: want %v, got %v", tc.wantNames, got)
			}
		})
	}
}

func TestAppliesToCluster(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		cluster     string
		want        bool
	}{
		{"absent_applies_to_all", nil, "a", true},
		{"empty_applies_to_none", map[string]string{AnnotationClusterScope: ""}, "a", false},
		{"whitespace_applies_to_none", map[string]string{AnnotationClusterScope: "  "}, "a", false},
		{"all_empty_entries_applies_to_none", map[string]string{AnnotationClusterScope: ",,"}, "a", false},
		{"single_match", map[string]string{AnnotationClusterScope: "a"}, "a", true},
		{"single_no_match", map[string]string{AnnotationClusterScope: "a"}, "b", false},
		{"multi_match", map[string]string{AnnotationClusterScope: "a,b"}, "b", true},
		{"multi_no_match", map[string]string{AnnotationClusterScope: "a,b"}, "c", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := appliesToCluster(tc.annotations, tc.cluster); got != tc.want {
				t.Errorf("want %v, got %v", tc.want, got)
			}
		})
	}
}

func TestUnknownClusterInScopeMessage_ByteExact(t *testing.T) {
	missing := []string{"c", "b"}
	applied := []string{"a"}
	sort.Strings(missing)
	sort.Strings(applied)
	got := formatUnknownClusterMessage(missing, applied)
	want := "curity.io/cluster references clusters not found in namespace: [b, c]. Config applied to existing clusters [a]. Missing clusters will apply if/when created."
	if got != want {
		t.Errorf("message drift detected (breaks EventRecorder dedup)\nwant: %q\ngot:  %q", want, got)
	}
}

func TestUnknownClusterInScopeMessage_AllNamesUnknown(t *testing.T) {
	// Edge case: every name in the scope annotation is a typo, so applied is
	// empty. formatNameList renders that as "[]" — the message text reads
	// awkwardly but is byte-stable, which is what EventRecorder dedup needs.
	// Locking it here so a future "fix" to the prose doesn't silently break
	// dedup.
	got := formatUnknownClusterMessage([]string{"only-typo"}, nil)
	want := "curity.io/cluster references clusters not found in namespace: [only-typo]. Config applied to existing clusters []. Missing clusters will apply if/when created."
	if got != want {
		t.Errorf("all-unknown message drift\nwant: %q\ngot:  %q", want, got)
	}
}

func TestUnknownClusterInScopeMessage_UnsortedInputProducesSameBytes(t *testing.T) {
	// Byte-stability guarantee must hold even if a caller forgets to sort.
	// formatNameList now sorts internally — a future caller that skips the
	// external sort should still get the same bytes, so EventRecorder dedup
	// can't be silently broken.
	sorted := formatUnknownClusterMessage([]string{"b", "c"}, []string{"a"})
	unsorted := formatUnknownClusterMessage([]string{"c", "b"}, []string{"a"})
	if sorted != unsorted {
		t.Errorf("formatUnknownClusterMessage must be order-insensitive (would break dedup)\nsorted:   %q\nunsorted: %q", sorted, unsorted)
	}
}

func TestFormatNameList_DoesNotMutateCaller(t *testing.T) {
	// The function sorts for byte-stability but must not mutate the caller's
	// slice — that would be an action-at-a-distance bug for anyone passing
	// a slice that's live elsewhere.
	input := []string{"c", "a", "b"}
	original := make([]string, len(input))
	copy(original, input)
	_ = formatNameList(input)
	for i := range input {
		if input[i] != original[i] {
			t.Errorf("input mutated: want %v, got %v", original, input)
			break
		}
	}
}

func TestFormatNameList_EmptySliceRendersAsEmptyBrackets(t *testing.T) {
	if got := formatNameList(nil); got != "[]" {
		t.Errorf("nil: want %q, got %q", "[]", got)
	}
	if got := formatNameList([]string{}); got != "[]" {
		t.Errorf("empty: want %q, got %q", "[]", got)
	}
}

func TestEmptyClusterScopeMessage_ByteExact(t *testing.T) {
	got := formatEmptyClusterScopeMessage()
	want := "curity.io/cluster annotation is empty; treating as applies-to-no-cluster. Remove the annotation to apply to all, or set a cluster list."
	if got != want {
		t.Errorf("message drift detected\nwant: %q\ngot:  %q", want, got)
	}
}

func TestEmitScopeAnnotationEvents_LandsOnCMNotCluster(t *testing.T) {
	// Core routing invariant: scope-issue Warning Events fire with the
	// offending ConfigMap (or Secret) as involvedObject, never the cluster
	// CR. UID-keyed EventRecorder dedup then collapses concurrent emissions
	// from N cluster reconcilers into one event series.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cluster,
		// Empty scope annotation.
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cm-empty", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: ""},
			},
		},
		// Unknown cluster in scope ("missing" doesn't exist; "this" does).
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cm-unknown", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "this,missing"},
			},
		},
	).Build()

	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.emitScopeAnnotationEvents(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 events, got %d: %+v", len(rec.events), rec.events)
	}

	// Index by reason for stable assertion regardless of emit order.
	byReason := map[string]capturedEvent{}
	for _, ev := range rec.events {
		byReason[ev.reason] = ev
	}

	emptyEv, ok := byReason[EventReasonEmptyClusterScope]
	if !ok {
		t.Fatalf("missing EmptyClusterScope event")
	}
	if cm, ok := emptyEv.object.(*corev1.ConfigMap); !ok || cm.Name != "cm-empty" {
		t.Errorf("EmptyClusterScope involvedObject: want ConfigMap/cm-empty, got %T %+v", emptyEv.object, emptyEv.object)
	}
	if emptyEv.message != formatEmptyClusterScopeMessage() {
		t.Errorf("EmptyClusterScope message drift\n want: %q\n got:  %q", formatEmptyClusterScopeMessage(), emptyEv.message)
	}

	unknownEv, ok := byReason[EventReasonUnknownClusterInScope]
	if !ok {
		t.Fatalf("missing UnknownClusterInScope event")
	}
	if cm, ok := unknownEv.object.(*corev1.ConfigMap); !ok || cm.Name != "cm-unknown" {
		t.Errorf("UnknownClusterInScope involvedObject: want ConfigMap/cm-unknown, got %T %+v", unknownEv.object, unknownEv.object)
	}
	wantUnknown := formatUnknownClusterMessage([]string{"missing"}, []string{"this"})
	if unknownEv.message != wantUnknown {
		t.Errorf("UnknownClusterInScope message drift\n want: %q\n got:  %q", wantUnknown, unknownEv.message)
	}

	// Regression guard: no events should land on the cluster CR — scope
	// issues belong on the offending CM/Secret so UID-keyed dedup works.
	for _, ev := range rec.events {
		if _, ok := ev.object.(*v1alpha1.IdentityServerCluster); ok {
			t.Errorf("event %q landed on the cluster CR — should land on the CM/Secret", ev.reason)
		}
	}
}

func TestEmitScopeAnnotationEvents_NoIssues_EmitsNoEvents(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		cluster,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cm-ok", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "this"},
			},
		},
	).Build()

	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.emitScopeAnnotationEvents(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(rec.events) != 0 {
		t.Errorf("expected no events, got %d: %+v", len(rec.events), rec.events)
	}
}

func TestDiscoverConfigResources_FiltersByScope(t *testing.T) {
	ctx := context.Background()
	s := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "applies", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "this"},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "other-scope", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: "other"},
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "no-scope", Namespace: "ns",
				Labels: map[string]string{LabelManagedConfig: "true"},
			},
		},
		// Empty-scope resources must NOT be mounted on any cluster. The
		// operator surfaces them via an EmptyClusterScope Warning Event on
		// the offending CM/Secret; discovery excludes them from the mount set.
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "empty-scope", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: ""},
			},
		},
	).Build()

	configs, _, err := discoverManagedResources(ctx, c, "ns", "this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 (applies + no-scope), got %d", len(configs))
	}
	names := []string{configs[0].Name, configs[1].Name}
	sort.Strings(names)
	if names[0] != "applies" || names[1] != "no-scope" {
		t.Errorf("expected [applies, no-scope], got %v", names)
	}
}

func TestEnsureManagedConfigDiscovery_ScopeScanFailure_ReturnsError(t *testing.T) {
	// When the scope scan fails (transient List error), the function returns
	// the error so controller-runtime requeues with backoff. No condition is
	// set (managed-config issues do not surface as conditions), no fallback
	// Event is emitted on the cluster, and no status is persisted from this
	// path.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns", Generation: 3},
	}

	listErr := errors.New("transient List failure")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, client client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*v1alpha1.IdentityServerClusterList); ok {
					return listErr
				}
				return client.List(ctx, list, opts...)
			},
		}).
		Build()

	rec := record.NewFakeRecorder(4)
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	err := r.ensureManagedConfigDiscovery(ctx, cluster)
	if err == nil {
		t.Fatalf("expected error when scope scan fails; got nil")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("expected wrapped List error, got: %v", err)
	}

	// Exactly one ScanFailed Warning Event must fire on the cluster CR —
	// operator-level failures have no specific resource to attach to.
	close(rec.Events)
	scanFailedCount := 0
	for e := range rec.Events {
		if strings.Contains(e, "Warning "+EventReasonScanFailed) {
			scanFailedCount++
			continue
		}
		t.Errorf("unexpected non-ScanFailed event on scan failure: %s", e)
	}
	if scanFailedCount != 1 {
		t.Errorf("expected exactly 1 ScanFailed event, got %d", scanFailedCount)
	}

	// No conditions are set by this code path — managed-config issues
	// surface as Events on the resource, not as conditions on the cluster.
	if len(cluster.Status.Conditions) != 0 {
		t.Errorf("expected no conditions set on scan failure, got: %+v", cluster.Status.Conditions)
	}
}

func TestEnsureManagedConfigDiscovery_DefaultConfigTypeAnnotationsFailure_EmitsOnCM(t *testing.T) {
	// A failed Update on a CM (Forbidden, webhook reject, server error)
	// must surface as a Warning Event on the offending CM, not stay silent
	// in operator logs only.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns", Generation: 1},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cm-forbidden", Namespace: "ns",
			Labels: map[string]string{LabelManagedConfig: "true"},
		},
	}
	updateErr := errors.New("forbidden on configmap update")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, cm).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok {
					return updateErr
				}
				return client.Update(ctx, obj, opts...)
			},
		}).
		Build()

	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	// Reconcile must NOT return an error — the failure is per-resource and
	// reconcile continues for other resources / for the rest of the
	// reconcile pipeline.
	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("expected nil error (continue-past-default-failure), got: %v", err)
	}

	// Find the ConfigTypeAnnotationDefault event on the offending CM.
	var found *capturedEvent
	for i := range rec.events {
		if rec.events[i].reason != "ConfigTypeAnnotationDefault" {
			continue
		}
		evCM, ok := rec.events[i].object.(*corev1.ConfigMap)
		if !ok || evCM.Name != "cm-forbidden" {
			t.Errorf("ConfigTypeAnnotationDefault event has wrong involvedObject: %T %+v", rec.events[i].object, rec.events[i].object)
			continue
		}
		found = &rec.events[i]
	}
	if found == nil {
		t.Fatalf("expected Warning ConfigTypeAnnotationDefault event on ConfigMap/cm-forbidden, got events: %+v", rec.events)
	}
	// Stable message — no rotating bytes from the underlying error — so
	// EventRecorder dedups a retry storm into one series per resource.
	if strings.Contains(found.message, updateErr.Error()) {
		t.Errorf("event message must NOT include raw error (breaks dedup); got: %q", found.message)
	}
	if !strings.Contains(found.message, "see operator logs") {
		t.Errorf("event message should point to operator logs; got: %q", found.message)
	}

	// Regression guard: the event must NOT land on the cluster CR —
	// managed-config events live on the resource, not the cluster.
	for _, ev := range rec.events {
		if ev.reason != "ConfigTypeAnnotationDefault" {
			continue
		}
		if _, ok := ev.object.(*v1alpha1.IdentityServerCluster); ok {
			t.Errorf("ConfigTypeAnnotationDefault must not land on the cluster CR; got %+v", ev)
		}
	}

	// Deliberate exclusion: this failure doesn't affect mount behaviour
	// (read-time defaulting picks base), so it stays out of status.
	for _, issue := range cluster.Status.ManagedResourceIssues {
		if issue.Reason == "ConfigTypeAnnotationDefault" {
			t.Errorf("ConfigTypeAnnotationDefault must NOT appear in status.managedResourceIssues; got %+v", issue)
		}
	}
	if cluster.Status.ManagedResourceIssueCount != len(cluster.Status.ManagedResourceIssues) {
		t.Errorf("count denormalized: count=%d len=%d",
			cluster.Status.ManagedResourceIssueCount, len(cluster.Status.ManagedResourceIssues))
	}
}

func TestEnsureManagedConfigDiscovery_DefaultConfigTypeAnnotationsFailure_OneBadDoesNotBlockOthers(t *testing.T) {
	// One CM whose Update fails must not block defaulting on the next CM.
	// Pre-fix the function returned on first failure; this regression-guards
	// the "continue past per-resource failure" behavior.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}
	// Two CMs — 'a-bad' fails Update, 'b-good' succeeds.
	cmBad := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "a-bad", Namespace: "ns",
			Labels: map[string]string{LabelManagedConfig: "true"},
		},
	}
	cmGood := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "b-good", Namespace: "ns",
			Labels: map[string]string{LabelManagedConfig: "true"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, cmBad, cmGood).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == "a-bad" {
					return errors.New("forbidden on a-bad")
				}
				return client.Update(ctx, obj, opts...)
			},
		}).
		Build()
	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// b-good must have been defaulted despite a-bad's failure.
	var liveGood corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Name: "b-good", Namespace: "ns"}, &liveGood); err != nil {
		t.Fatalf("re-fetch b-good: %v", err)
	}
	if liveGood.Annotations[AnnotationConfigType] != ConfigTypeBase {
		t.Errorf("b-good must have been defaulted to %q despite a-bad's failure; got annotation = %q",
			ConfigTypeBase, liveGood.Annotations[AnnotationConfigType])
	}

	// Exactly one event (on a-bad).
	defaultEvents := 0
	for _, ev := range rec.events {
		if ev.reason == "ConfigTypeAnnotationDefault" {
			defaultEvents++
			if cm, ok := ev.object.(*corev1.ConfigMap); !ok || cm.Name != "a-bad" {
				t.Errorf("event on wrong resource: %T %+v", ev.object, ev.object)
			}
		}
	}
	if defaultEvents != 1 {
		t.Errorf("expected exactly 1 ConfigTypeAnnotationDefault event (on a-bad), got %d", defaultEvents)
	}
}

func TestEnsureManagedConfigDiscovery_DefaultConfigTypeAnnotationsFailure_EmitsOnSecret(t *testing.T) {
	// Same shape as the CM test, for Secrets — verifies the Kind disambiguator
	// works when the failing resource is a Secret.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "secret-forbidden", Namespace: "ns",
			Labels: map[string]string{LabelManagedConfig: "true"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, sec).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, client client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.New("forbidden on secret update")
				}
				return client.Update(ctx, obj, opts...)
			},
		}).
		Build()
	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, ev := range rec.events {
		if ev.reason != "ConfigTypeAnnotationDefault" {
			continue
		}
		if evSec, ok := ev.object.(*corev1.Secret); ok && evSec.Name == "secret-forbidden" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected Warning ConfigTypeAnnotationDefault on Secret/secret-forbidden; got: %+v", rec.events)
	}
}

func TestEmitScopeAnnotationEvents_TrueToFalse_NoEventsAfterFix(t *testing.T) {
	// After a ConfigMap with a scope issue is fixed (deleted or re-scoped),
	// re-running emit must produce no further Warning events on the
	// resource. The next reconcile finds nothing to flag; existing event
	// rows in the API server age out via TTL on their own.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns", Generation: 1},
	}
	badCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cm-unknown", Namespace: "ns",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: map[string]string{AnnotationClusterScope: "this,missing"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(cluster, badCM).Build()

	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	// First pass: scope issue present → one UnknownClusterInScope on the CM.
	if err := r.emitScopeAnnotationEvents(ctx, cluster); err != nil {
		t.Fatalf("first pass: unexpected error: %v", err)
	}
	if len(rec.events) != 1 || rec.events[0].reason != EventReasonUnknownClusterInScope {
		t.Fatalf("first pass: expected one UnknownClusterInScope event, got %+v", rec.events)
	}

	// Delete the offending ConfigMap and re-run.
	if err := c.Delete(ctx, badCM); err != nil {
		t.Fatalf("deleting bad configmap: %v", err)
	}
	rec.events = nil

	if err := r.emitScopeAnnotationEvents(ctx, cluster); err != nil {
		t.Fatalf("second pass: unexpected error: %v", err)
	}
	if len(rec.events) != 0 {
		t.Errorf("second pass: expected no events after fix, got %+v", rec.events)
	}
}

// --- Status.ManagedResourceIssues population ---

// runDiscoveryWithObjs builds a fake client + reconciler and runs
// ensureManagedConfigDiscovery once, returning the resulting issues + count.
func runDiscoveryWithObjs(t *testing.T, clusterName string, objs ...client.Object) ([]v1alpha1.ManagedResourceIssue, int) {
	t.Helper()
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: "ns"},
	}
	all := append([]client.Object{cluster}, objs...)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(all...).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		Build()
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: &capturingRecorder{},
	}
	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("ensureManagedConfigDiscovery: %v", err)
	}
	return cluster.Status.ManagedResourceIssues, cluster.Status.ManagedResourceIssueCount
}

func managedCM(name, ns string, ann map[string]string, data map[string]string) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: ann,
		},
	}
	if data != nil {
		cm.Data = data
	}
	return cm
}

func managedSecret(name, ns string, ann map[string]string, data map[string][]byte) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: ann,
		},
	}
	if data != nil {
		s.Data = data
	}
	return s
}

func TestManagedResourceIssues_NoIssues_EmptyAndZeroCount(t *testing.T) {
	// Healthy CM that applies to this cluster: no issue → empty status.
	cm := managedCM("ok-cm", "ns",
		map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
		map[string]string{"x.xml": "<x/>"})

	issues, count := runDiscoveryWithObjs(t, "this", cm)

	if len(issues) != 0 {
		t.Errorf("expected empty ManagedResourceIssues, got %+v", issues)
	}
	if count != 0 {
		t.Errorf("expected ManagedResourceIssueCount=0, got %d", count)
	}
}

func TestManagedResourceIssues_UnknownConfigType_PopulatedForCM(t *testing.T) {
	cm := managedCM("bogus-cm", "ns",
		map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
		map[string]string{"x.xml": "<x/>"})

	issues, count := runDiscoveryWithObjs(t, "this", cm)

	if count != 1 || len(issues) != 1 {
		t.Fatalf("expected 1 issue, got count=%d issues=%+v", count, issues)
	}
	got := issues[0]
	if got.Kind != "ConfigMap" || got.Name != "bogus-cm" || got.Reason != "UnknownConfigType" {
		t.Errorf("unexpected issue fields: %+v", got)
	}
	// Message must be byte-stable (operator-built, no rotating bytes from runtime errors)
	// and identify the bad value verbatim so users can locate the typo.
	if !strings.Contains(got.Message, "bogus") {
		t.Errorf("message should name the bad value: %q", got.Message)
	}
}

func TestManagedResourceIssues_UnknownConfigType_PopulatedForSecret_KindCorrect(t *testing.T) {
	// Verifies the Kind disambiguator: a Secret with bad config-type appears
	// as Kind=Secret in status, not ConfigMap.
	sec := managedSecret("bogus-secret", "ns",
		map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
		map[string][]byte{"x.xml": []byte("<x/>")})

	issues, count := runDiscoveryWithObjs(t, "this", sec)

	if count != 1 || len(issues) != 1 {
		t.Fatalf("expected 1 issue, got count=%d issues=%+v", count, issues)
	}
	got := issues[0]
	if got.Kind != "Secret" {
		t.Errorf("Kind: want Secret, got %q (Kind disambiguates CM vs Secret in API)", got.Kind)
	}
	if got.Name != "bogus-secret" || got.Reason != "UnknownConfigType" {
		t.Errorf("unexpected issue fields: %+v", got)
	}
}

func TestManagedResourceIssues_DuplicateConfigKey_BothCollidersIncluded(t *testing.T) {
	// Two CMs sharing a data key + config-type. Each colliding resource
	// gets its own status entry — same key collision implicates both.
	a := managedCM("dup-a", "ns",
		map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
		map[string]string{"shared.xml": "<a/>"})
	b := managedCM("dup-b", "ns",
		map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
		map[string]string{"shared.xml": "<b/>"})

	issues, count := runDiscoveryWithObjs(t, "this", a, b)

	if count != 2 || len(issues) != 2 {
		t.Fatalf("expected 2 issues (one per collider), got count=%d issues=%+v", count, issues)
	}
	for _, i := range issues {
		if i.Reason != "DuplicateConfigKey" {
			t.Errorf("expected Reason=DuplicateConfigKey, got %+v", i)
		}
		if i.Kind != "ConfigMap" {
			t.Errorf("expected Kind=ConfigMap, got %+v", i)
		}
	}
	names := []string{issues[0].Name, issues[1].Name}
	sort.Strings(names)
	if names[0] != "dup-a" || names[1] != "dup-b" {
		t.Errorf("expected colliders [dup-a, dup-b], got %v", names)
	}
}

func TestManagedResourceIssues_DuplicateConfigKey_AcrossCMAndSecret(t *testing.T) {
	// CM + Secret with the same data key + config-type → both implicated,
	// each with the right Kind.
	cm := managedCM("dup-cm", "ns",
		map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
		map[string]string{"shared.xml": "<cm/>"})
	sec := managedSecret("dup-sec", "ns",
		map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
		map[string][]byte{"shared.xml": []byte("<s/>")})

	issues, count := runDiscoveryWithObjs(t, "this", cm, sec)

	if count != 2 || len(issues) != 2 {
		t.Fatalf("expected 2 issues, got count=%d issues=%+v", count, issues)
	}
	byKind := map[string]v1alpha1.ManagedResourceIssue{}
	for _, i := range issues {
		byKind[i.Kind] = i
	}
	if byKind["ConfigMap"].Name != "dup-cm" {
		t.Errorf("ConfigMap entry: want dup-cm, got %+v", byKind["ConfigMap"])
	}
	if byKind["Secret"].Name != "dup-sec" {
		t.Errorf("Secret entry: want dup-sec, got %+v", byKind["Secret"])
	}
}

func TestManagedResourceIssues_ScopeOnlyIssuesExcluded(t *testing.T) {
	// Regression guard for the per-cluster relevance rule: scope-only issues
	// (UnknownClusterInScope, EmptyClusterScope) DO NOT appear in
	// status.ManagedResourceIssues. They surface only as Events on the
	// resource because they don't change what mounts on this cluster.
	emptyScope := managedCM("empty-cm", "ns",
		map[string]string{AnnotationClusterScope: ""},
		map[string]string{"x.xml": "<x/>"})
	partialUnknown := managedCM("typo-cm", "ns",
		map[string]string{AnnotationClusterScope: "this,does-not-exist", AnnotationConfigType: ConfigTypeBase},
		map[string]string{"y.xml": "<y/>"})

	issues, count := runDiscoveryWithObjs(t, "this", emptyScope, partialUnknown)

	if count != 0 || len(issues) != 0 {
		t.Errorf("scope-only issues must NOT appear in status; got count=%d issues=%+v", count, issues)
	}
}

func TestManagedResourceIssues_CompoundIssue_OnlyConfigTypeInStatus(t *testing.T) {
	// A CM with both bad scope (typo) AND bad config-type. Per the rules,
	// only the config-type issue (which actually denies the cluster a config)
	// belongs in status. The scope-typo is event-only.
	cm := managedCM("compound", "ns",
		map[string]string{
			AnnotationClusterScope: "this,does-not-exist",
			AnnotationConfigType:   "bogus",
		},
		map[string]string{"x.xml": "<x/>"})

	issues, count := runDiscoveryWithObjs(t, "this", cm)

	if count != 1 || len(issues) != 1 {
		t.Fatalf("expected exactly 1 issue (UnknownConfigType only), got count=%d issues=%+v", count, issues)
	}
	if issues[0].Reason != "UnknownConfigType" {
		t.Errorf("expected only UnknownConfigType, got %+v (scope-typo must NOT appear in status)", issues[0])
	}
}

func TestManagedResourceIssues_SortedByKindNameReason(t *testing.T) {
	// Stable sort matters: status JSON comparison in tests, snapshot diffs,
	// and human readability. Order: Kind first, then Name, then Reason.
	objs := []client.Object{
		// Two CMs sharing a key (DuplicateConfigKey on both)
		managedCM("z-cm", "ns",
			map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
			map[string]string{"k.xml": "<a/>"}),
		managedCM("a-cm", "ns",
			map[string]string{AnnotationConfigType: ConfigTypeBase, AnnotationClusterScope: "this"},
			map[string]string{"k.xml": "<b/>"}),
		// One Secret with bad config-type
		managedSecret("m-secret", "ns",
			map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
			map[string][]byte{"x.xml": []byte("<x/>")}),
	}

	issues, _ := runDiscoveryWithObjs(t, "this", objs...)

	if len(issues) != 3 {
		t.Fatalf("expected 3 issues, got %d: %+v", len(issues), issues)
	}
	// Expected order: ConfigMap/a-cm, ConfigMap/z-cm, Secret/m-secret
	want := []struct{ kind, name string }{
		{"ConfigMap", "a-cm"},
		{"ConfigMap", "z-cm"},
		{"Secret", "m-secret"},
	}
	for i, w := range want {
		if issues[i].Kind != w.kind || issues[i].Name != w.name {
			t.Errorf("position %d: want %s/%s, got %s/%s",
				i, w.kind, w.name, issues[i].Kind, issues[i].Name)
		}
	}
}

func TestManagedResourceIssues_CountMatchesSliceLength(t *testing.T) {
	// Count is a sibling scalar exposed via a printer column — it MUST always
	// equal len(Issues). Drift would make `kubectl get isc` lie about the
	// number of issues a cluster has.
	cms := []client.Object{}
	for i := 0; i < 4; i++ {
		cms = append(cms, managedCM(fmt.Sprintf("bad-%d", i), "ns",
			map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
			map[string]string{"x.xml": "<x/>"}))
	}

	issues, count := runDiscoveryWithObjs(t, "this", cms...)

	if count != len(issues) {
		t.Errorf("Count must equal len(Issues): got count=%d len=%d", count, len(issues))
	}
	if count != 4 {
		t.Errorf("expected 4 (one per bad CM), got %d", count)
	}
}

func TestManagedResourceIssues_Idempotent_TwoReconcilesSameOutput(t *testing.T) {
	// Reconciles run repeatedly. The status must be a function of namespace
	// state, not accumulate from prior runs. A bug like "append without
	// reset" would inflate Issues across reconciles even when nothing
	// changed.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}
	cm := managedCM("bogus-cm", "ns",
		map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
		map[string]string{"x.xml": "<x/>"})
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, cm).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		Build()
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: &capturingRecorder{},
	}

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := append([]v1alpha1.ManagedResourceIssue(nil), cluster.Status.ManagedResourceIssues...)
	firstCount := cluster.Status.ManagedResourceIssueCount

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	second := cluster.Status.ManagedResourceIssues
	secondCount := cluster.Status.ManagedResourceIssueCount

	if firstCount != secondCount {
		t.Errorf("Count drifted across reconciles: first=%d second=%d", firstCount, secondCount)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("Issues drifted across reconciles\nfirst:  %+v\nsecond: %+v", first, second)
	}
}

func TestManagedResourceIssues_OutOfScopeResourceExcluded(t *testing.T) {
	// A bad CM scoped to a DIFFERENT cluster must NOT show up in this
	// cluster's status. The relevance filter is the core per-cluster rule.
	otherClusterCM := managedCM("for-other", "ns",
		map[string]string{
			AnnotationClusterScope: "other-cluster",
			AnnotationConfigType:   "bogus",
		},
		map[string]string{"x.xml": "<x/>"})

	issues, count := runDiscoveryWithObjs(t, "this", otherClusterCM)

	if count != 0 || len(issues) != 0 {
		t.Errorf("CM scoped to another cluster must NOT appear in this cluster's status; got %+v", issues)
	}
}

func TestManagedResourceIssues_ClearedWhenAllResourcesFixed(t *testing.T) {
	// First pass: bad CM → 1 issue. Then fix the CM (set valid config-type)
	// and re-run: status must drop the issue. A bug like "never clear" would
	// leave stale entries.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}
	cm := managedCM("toggling", "ns",
		map[string]string{AnnotationConfigType: "bogus", AnnotationClusterScope: "this"},
		map[string]string{"x.xml": "<x/>"})
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster, cm).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		Build()
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: &capturingRecorder{},
	}

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if cluster.Status.ManagedResourceIssueCount != 1 {
		t.Fatalf("first pass: expected 1 issue, got %d", cluster.Status.ManagedResourceIssueCount)
	}

	// Fix the CM's config-type.
	var live corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Name: "toggling", Namespace: "ns"}, &live); err != nil {
		t.Fatalf("re-fetch CM: %v", err)
	}
	live.Annotations[AnnotationConfigType] = ConfigTypeBase
	if err := c.Update(ctx, &live); err != nil {
		t.Fatalf("update CM: %v", err)
	}

	if err := r.ensureManagedConfigDiscovery(ctx, cluster); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if cluster.Status.ManagedResourceIssueCount != 0 {
		t.Errorf("second pass after fix: expected 0 issues, got count=%d issues=%+v",
			cluster.Status.ManagedResourceIssueCount, cluster.Status.ManagedResourceIssues)
	}
	if len(cluster.Status.ManagedResourceIssues) != 0 {
		t.Errorf("Issues slice not cleared after fix: %+v", cluster.Status.ManagedResourceIssues)
	}
}

// --- N-cluster dedup precondition ---
//
// EventRecorder dedups when (involvedObject.UID, source, reason, message)
// match. Unit tests assert the precondition: every reconciler passes the
// same Object pointer + reason + byte-identical message.

func TestEnsureManagedConfigDiscovery_ListFailure_EmitsScanFailedOnCluster(t *testing.T) {
	// Operator-level List failures have no specific resource to attach to,
	// so ScanFailed lands on the cluster CR.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns"},
	}
	listErr := errors.New("forbidden listing configmaps")
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, client client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.ConfigMapList); ok {
					return listErr
				}
				return client.List(ctx, list, opts...)
			},
		}).
		Build()
	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{Client: c, Scheme: s, Recorder: rec}

	err := r.ensureManagedConfigDiscovery(ctx, cluster)
	if err == nil {
		t.Fatalf("expected error from List failure, got nil")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("expected wrapped List error, got: %v", err)
	}

	// Find the ScanFailed event on the cluster CR.
	var found *capturedEvent
	for i := range rec.events {
		if rec.events[i].reason != EventReasonScanFailed {
			continue
		}
		if _, ok := rec.events[i].object.(*v1alpha1.IdentityServerCluster); ok {
			found = &rec.events[i]
		}
	}
	if found == nil {
		t.Fatalf("expected Warning ScanFailed event on cluster CR, got events: %+v", rec.events)
	}
	// Stable message — no rotating bytes from the underlying error — so
	// EventRecorder dedups a retry storm into one series.
	if strings.Contains(found.message, listErr.Error()) {
		t.Errorf("ScanFailed event must NOT include raw error (breaks dedup); got: %q", found.message)
	}
	if !strings.Contains(found.message, "see operator logs") {
		t.Errorf("ScanFailed event should point to operator logs; got: %q", found.message)
	}
}

func TestEmitScopeAnnotationEvents_NClusterReconcilers_AllEmitOnSameKey(t *testing.T) {
	// Verify each cluster's reconciler emits on the same CM UID, same
	// reason, byte-identical message — the dedup precondition.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	clusterA := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "ns", UID: "uid-a"}}
	clusterB := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns", UID: "uid-b"}}
	clusterC := &v1alpha1.IdentityServerCluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", UID: "uid-c"}}
	// Bad CM: scope=a,b,does-not-exist → UnknownClusterInScope on this CM.
	// Applies to clusters a and b (so their reconcilers find it via scope).
	// Cluster c will also see the issue when it scans namespace-wide.
	badCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-cm", Namespace: "ns", UID: "cm-uid",
			Labels:      map[string]string{LabelManagedConfig: "true"},
			Annotations: map[string]string{AnnotationClusterScope: "a,b,does-not-exist"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(clusterA, clusterB, clusterC, badCM).Build()
	rec := &capturingRecorder{}
	r := &IdentityServerClusterReconciler{Client: c, Scheme: s, Recorder: rec}

	for _, cl := range []*v1alpha1.IdentityServerCluster{clusterA, clusterB, clusterC} {
		if err := r.emitScopeAnnotationEvents(ctx, cl); err != nil {
			t.Fatalf("cluster %s: %v", cl.Name, err)
		}
	}

	// Filter to the events relevant to our bad CM.
	var emits []capturedEvent
	for _, ev := range rec.events {
		if ev.reason != EventReasonUnknownClusterInScope {
			continue
		}
		emits = append(emits, ev)
	}
	if len(emits) != 3 {
		t.Fatalf("expected 3 UnknownClusterInScope emissions (one per cluster reconciler), got %d: %+v", len(emits), emits)
	}

	// All three emissions must point at the SAME ConfigMap (same UID).
	// Without this, server-side dedup would treat them as distinct events.
	var uids []string
	for _, ev := range emits {
		cm, ok := ev.object.(*corev1.ConfigMap)
		if !ok {
			t.Fatalf("expected involvedObject *corev1.ConfigMap, got %T %+v", ev.object, ev.object)
		}
		uids = append(uids, string(cm.UID))
	}
	for i := 1; i < len(uids); i++ {
		if uids[i] != uids[0] {
			t.Errorf("involvedObject UIDs differ across reconcilers: [%s, %s, ...] — server-side dedup requires identical UID",
				uids[0], uids[i])
		}
	}

	// All three emissions must have byte-identical messages. Server-side
	// dedup keys on message bytes; even one byte of drift produces a new
	// event series.
	for i := 1; i < len(emits); i++ {
		if emits[i].message != emits[0].message {
			t.Errorf("message bytes differ across reconcilers (breaks dedup):\n[0]: %q\n[%d]: %q",
				emits[0].message, i, emits[i].message)
		}
	}

	// All three must point at the SAME live ConfigMap pointer too — the
	// recorder is value-by-pointer here so this proves we're not creating
	// fresh runtime.Objects per reconcile (which would have the same UID
	// but different in-process identity, fine for k8s but worth pinning).
	for i := 1; i < len(emits); i++ {
		// Compare by Name+Namespace since the fake client may give back
		// distinct pointers for the same object across List calls.
		a, _ := emits[0].object.(*corev1.ConfigMap)
		b, _ := emits[i].object.(*corev1.ConfigMap)
		if a.Name != b.Name || a.Namespace != b.Namespace {
			t.Errorf("involvedObject identity differs: [0]=%s/%s vs [%d]=%s/%s",
				a.Namespace, a.Name, i, b.Namespace, b.Name)
		}
	}
}

// --- Stale ConfigScopeIssues condition cleanup ---

func TestReconcile_RemovesStaleConfigScopeIssuesCondition(t *testing.T) {
	// Pre-seed a stale ConfigScopeIssues condition (from an older operator)
	// and verify it's dropped after one reconcile while operator-owned
	// conditions survive.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "stale", Namespace: "ns", Generation: 1,
			Finalizers: []string{v1alpha1.ClusterFinalizer}, // skip finalizer-add path
		},
		Spec: v1alpha1.IdentityServerClusterSpec{Version: "11.0"},
		Status: v1alpha1.IdentityServerClusterStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "ConfigScopeIssues",
					Status:             metav1.ConditionTrue,
					Reason:             "UnknownClusters",
					Message:            "[stale entry from older operator]",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{}).
		Build()
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: &capturingRecorder{},
	}

	// Run Reconcile. With no admin node and no managed configs, ensureClusterConfig
	// short-circuits to WaitingForAdmin and ensureManagedConfigDiscovery does
	// nothing — Reconcile reaches the final Status().Update with the cleanup
	// applied.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// Re-fetch the cluster from the (fake) API server and verify persisted state.
	var fresh v1alpha1.IdentityServerCluster
	if err := c.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}

	for _, cond := range fresh.Status.Conditions {
		if cond.Type == "ConfigScopeIssues" {
			t.Errorf("stale ConfigScopeIssues condition was not cleaned up: %+v", cond)
		}
	}

	// Operator-owned conditions that remain valid must still be set.
	hasReady := false
	for _, cond := range fresh.Status.Conditions {
		if cond.Type == v1alpha1.ConditionReady {
			hasReady = true
		}
	}
	if !hasReady {
		t.Errorf("expected operator-computed Ready condition after reconcile; got %+v", fresh.Status.Conditions)
	}
}
