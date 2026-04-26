package controller

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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

func TestUpdateConfigScopeIssuesCondition_EmitsEvents(t *testing.T) {
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

	rec := record.NewFakeRecorder(8)
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.updateConfigScopeIssuesCondition(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With both Empty and Unknown issues present, the OR branch in
	// updateConfigScopeIssuesCondition sets reason=UnknownClusters (it only
	// falls into EmptyScope when Unknown is absent). Verify the condition
	// itself is set correctly, not just the emitted events.
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConfigScopeIssues)
	if cond == nil {
		t.Fatalf("expected ConfigScopeIssues condition to be set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition status: want True, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonUnknownClusters {
		t.Errorf("condition reason (mixed Empty+Unknown must pick UnknownClusters): want %s, got %s",
			v1alpha1.ReasonUnknownClusters, cond.Reason)
	}

	close(rec.Events)
	var got []string
	for e := range rec.Events {
		got = append(got, e)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d: %v", len(got), got)
	}

	wantEmpty := "Warning " + EventReasonEmptyClusterScope + " " +
		formatEmptyClusterScopeMessage() + " on ConfigMap/cm-empty"
	wantUnknown := "Warning " + EventReasonUnknownClusterInScope + " " +
		formatUnknownClusterMessage([]string{"missing"}, []string{"this"}) +
		" on ConfigMap/cm-unknown"

	sort.Strings(got)
	want := []string{wantEmpty, wantUnknown}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event mismatch\nwant:\n  %s\ngot:\n  %s", strings.Join(want, "\n  "), strings.Join(got, "\n  "))
	}
}

func TestUpdateConfigScopeIssuesCondition_NoIssues_EmitsNoEvents(t *testing.T) {
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

	rec := record.NewFakeRecorder(4)
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	if err := r.updateConfigScopeIssuesCondition(ctx, cluster); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	close(rec.Events)
	for e := range rec.Events {
		t.Errorf("expected no events, got: %s", e)
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
		// operator flags them via ConfigScopeIssues; discovery excludes them.
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "empty-scope", Namespace: "ns",
				Labels:      map[string]string{LabelManagedConfig: "true"},
				Annotations: map[string]string{AnnotationClusterScope: ""},
			},
		},
	).Build()

	configs, _, err := discoverConfigResources(ctx, c, "ns", "this")
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

func TestEnsureManagedConfigDiscovery_ScopeScanFailure_SetsUnknownEmitsEventReturnsError(t *testing.T) {
	// Regression: when updateConfigScopeIssuesCondition fails (transient List
	// error), the discovery path previously just logged and continued —
	// leaving a stale False/NoIssues condition in etcd. It must now flip the
	// condition to Unknown/ScanFailed, emit a Warning, persist status, and
	// return the error so reconcile requeues.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "this", Namespace: "ns", Generation: 3,
		},
		// Pre-existing stale False/NoIssues — the bug was that this stuck around
		// after a failed scan. Verify it flips to Unknown.
		Status: v1alpha1.IdentityServerClusterStatus{
			Conditions: []metav1.Condition{{
				Type:    v1alpha1.ConditionConfigScopeIssues,
				Status:  metav1.ConditionFalse,
				Reason:  v1alpha1.ReasonNoIssues,
				Message: "No scope annotation issues observed",
			}},
		},
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

	// Condition must now be Unknown/ScanFailed (in-memory).
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConfigScopeIssues)
	if cond == nil {
		t.Fatalf("expected ConfigScopeIssues condition to be set")
	}
	if cond.Status != metav1.ConditionUnknown {
		t.Errorf("condition status: want Unknown, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonScanFailed {
		t.Errorf("condition reason: want ScanFailed, got %s", cond.Reason)
	}
	if cond.ObservedGeneration != cluster.Generation {
		t.Errorf("observedGeneration: want %d, got %d", cluster.Generation, cond.ObservedGeneration)
	}

	// Condition must be persisted — fetch the cluster back and re-check.
	var fresh v1alpha1.IdentityServerCluster
	if getErr := c.Get(ctx, client.ObjectKeyFromObject(cluster), &fresh); getErr != nil {
		t.Fatalf("re-fetching cluster: %v", getErr)
	}
	persisted := apimeta.FindStatusCondition(fresh.Status.Conditions, v1alpha1.ConditionConfigScopeIssues)
	if persisted == nil || persisted.Status != metav1.ConditionUnknown ||
		persisted.Reason != v1alpha1.ReasonScanFailed {
		t.Errorf("persisted condition: want Unknown/ScanFailed, got %+v", persisted)
	}

	// A Warning event must have been emitted so users see the failure.
	close(rec.Events)
	var events []string
	for e := range rec.Events {
		events = append(events, e)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(events), events)
	}
	if !strings.Contains(events[0], "Warning "+v1alpha1.ReasonScanFailed) {
		t.Errorf("event type/reason: want Warning/ScanFailed, got: %s", events[0])
	}
	// The event message is intentionally stable (no raw err interpolation)
	// so EventRecorder dedups a retry loop into a single series. Verify
	// the bytes do NOT vary with the underlying error.
	if strings.Contains(events[0], listErr.Error()) {
		t.Errorf("event must NOT include raw error (breaks dedup); got: %s", events[0])
	}
	if !strings.Contains(events[0], "see operator logs") {
		t.Errorf("event should instruct operator to check logs, got: %s", events[0])
	}
}

func TestEnsureManagedConfigDiscovery_ScopeScanFailure_StatusPersistAlsoFails_JoinsBothErrors(t *testing.T) {
	// When BOTH the scope scan and the subsequent Status().Update fail, the
	// returned error must carry both — otherwise the persist failure gets
	// swallowed into log-only output and only the scan error shows up in
	// the controller-runtime error chain. errors.Is must match each cause.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns", Generation: 1},
	}

	listErr := errors.New("transient List failure")
	statusErr := errors.New("500 from apiserver on status update")
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
			SubResourceUpdate: func(ctx context.Context, client client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return statusErr
				}
				return client.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}).
		Build()

	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(4),
	}

	err := r.ensureManagedConfigDiscovery(ctx, cluster)
	if err == nil {
		t.Fatalf("expected error when both scan and status persist fail")
	}
	if !errors.Is(err, listErr) {
		t.Errorf("expected returned error to wrap scan error, got: %v", err)
	}
	if !errors.Is(err, statusErr) {
		t.Errorf("expected returned error to wrap status-update error, got: %v", err)
	}
}

func TestEnsureManagedConfigDiscovery_DefaultConfigTypeAnnotationsFailure_EmitsWarningEvent(t *testing.T) {
	// When defaultConfigTypeAnnotations fails (e.g. per-resource RBAC or a
	// conflict on one CM), the reconciler log-and-continues so reconcile
	// makes progress — but without a user-visible signal, partial-write
	// state is invisible to anyone running kubectl describe. A stable
	// Warning event surfaces it. Stable message = one dedup'd Event even
	// under a retry loop.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "this", Namespace: "ns", Generation: 1},
	}
	// An unannotated managed CM — default-annotation loop will try to write
	// to it, the interceptor will fail that write.
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

	rec := record.NewFakeRecorder(8)
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	// Call via ensureManagedConfigDiscovery (the production entry point) so we
	// also exercise the continue-past-default-failure control flow.
	_ = r.ensureManagedConfigDiscovery(ctx, cluster)

	close(rec.Events)
	found := false
	for e := range rec.Events {
		if strings.Contains(e, "Warning ConfigTypeAnnotationDefault") {
			found = true
			// Event message must NOT include the raw error — that would
			// defeat EventRecorder dedup on transient/rotating errors.
			if strings.Contains(e, updateErr.Error()) {
				t.Errorf("event must not embed raw error (breaks dedup): %s", e)
			}
			if !strings.Contains(e, "see operator logs") {
				t.Errorf("event should point at operator logs: %s", e)
			}
		}
	}
	if !found {
		t.Fatalf("expected Warning/ConfigTypeAnnotationDefault event, got none")
	}
}

func TestUpdateConfigScopeIssuesCondition_TrueToFalse_ClearsWhenBadConfigMapDeleted(t *testing.T) {
	// Regression: after a ConfigMap with a scope issue is deleted, the
	// cluster's ConfigScopeIssues condition must flip back to False/NoIssues.
	// Only the entry path (transition into True) was previously tested.
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

	rec := record.NewFakeRecorder(16)
	r := &IdentityServerClusterReconciler{
		Client:   c,
		Scheme:   s,
		Recorder: rec,
	}

	// First pass: scope issue present → condition must be True/UnknownClusters.
	if err := r.updateConfigScopeIssuesCondition(ctx, cluster); err != nil {
		t.Fatalf("first pass: unexpected error: %v", err)
	}
	cond := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConfigScopeIssues)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != v1alpha1.ReasonUnknownClusters {
		t.Fatalf("expected True/UnknownClusters after first pass, got: %+v", cond)
	}

	// Delete the offending ConfigMap and re-run the scan.
	if err := c.Delete(ctx, badCM); err != nil {
		t.Fatalf("deleting bad configmap: %v", err)
	}

	if err := r.updateConfigScopeIssuesCondition(ctx, cluster); err != nil {
		t.Fatalf("second pass: unexpected error: %v", err)
	}

	// Condition must now be False/NoIssues — the core clearing behavior.
	cond = apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionConfigScopeIssues)
	if cond == nil {
		t.Fatalf("expected ConfigScopeIssues condition to remain set after clear")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("condition status after clear: want False, got %s", cond.Status)
	}
	if cond.Reason != v1alpha1.ReasonNoIssues {
		t.Errorf("condition reason after clear: want NoIssues, got %s", cond.Reason)
	}

	// No Warning events should fire on the clean pass — only the first pass
	// should have emitted. Drain and count only events from the second pass
	// by counting all and subtracting what we saw from pass one (which emits
	// one UnknownClusterInScope event).
	close(rec.Events)
	total := 0
	for range rec.Events {
		total++
	}
	if total != 1 {
		t.Errorf("expected exactly 1 event total (from first pass only), got %d", total)
	}
}
