package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// schemeWithBatch returns a scheme with corev1 + batchv1 registered.
// Used by job-pod-admission tests that need to fake batch/v1.Job objects.
func schemeWithBatch(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := newScheme(t)
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("adding batchv1 to scheme: %v", err)
	}
	return s
}

func newNodeReconcilerForHelperTest(t *testing.T, node *v1alpha1.IdentityServerNode, statusUpdateErr error) (*IdentityServerNodeReconciler, *record.FakeRecorder) {
	t.Helper()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(node).
		WithStatusSubresource(&v1alpha1.IdentityServerNode{})

	if statusUpdateErr != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return statusUpdateErr
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		})
	}

	rec := record.NewFakeRecorder(8)
	return &IdentityServerNodeReconciler{
		Client:   builder.Build(),
		Scheme:   s,
		Recorder: rec,
	}, rec
}

func freshTestNode() *v1alpha1.IdentityServerNode {
	return &v1alpha1.IdentityServerNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rt-1",
			Namespace:  "ns",
			Generation: 7,
		},
	}
}

func invalidErr(message string) error {
	gk := schema.GroupKind{Group: "apps", Kind: "Deployment"}
	return apierrors.NewInvalid(gk, "rt-1", field.ErrorList{
		field.Invalid(field.NewPath("spec", "template", "metadata", "labels"), "bad/label", message),
	})
}

func TestHandlePermanentWriteError_IsInvalid_FlipsConditions(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()
	r, rec := newNodeReconcilerForHelperTest(t, node, nil)

	res, handled, err := r.handlePermanentWriteError(ctx, node, invalidErr("invalid label syntax"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected zero-value Result on permanent-error success path, got %+v", res)
	}

	deg := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Degraded condition wrong: %+v", deg)
	}
	if deg != nil && !strings.Contains(deg.Message, "Deployment rejected by apiserver:") {
		t.Errorf("Degraded message lacks expected prefix: %q", deg.Message)
	}
	if deg != nil && deg.ObservedGeneration != 7 {
		t.Errorf("Degraded ObservedGeneration = %d, want 7", deg.ObservedGeneration)
	}

	ready := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Ready condition wrong: %+v", ready)
	}

	if node.Status.ObservedGeneration != 7 {
		t.Errorf("top-level ObservedGeneration = %d, want 7", node.Status.ObservedGeneration)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning "+v1alpha1.ReasonInvalidSpec) {
			t.Errorf("unexpected event: %q", ev)
		}
	default:
		t.Error("expected one Warning event, got none")
	}
}

func TestHandlePermanentWriteError_IsConflict_NotHandled(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()
	r, _ := newNodeReconcilerForHelperTest(t, node, nil)

	conflictErr := apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "rt-1", errors.New("the object has been modified"))

	_, handled, err := r.handlePermanentWriteError(ctx, node, conflictErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for IsConflict")
	}
	if len(node.Status.Conditions) != 0 {
		t.Errorf("expected no conditions touched, got: %+v", node.Status.Conditions)
	}
}

func TestHandlePermanentWriteError_IsNotFound_NotHandled(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()
	r, _ := newNodeReconcilerForHelperTest(t, node, nil)

	nfErr := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "rt-1")

	_, handled, err := r.handlePermanentWriteError(ctx, node, nfErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for IsNotFound")
	}
}

func TestHandlePermanentWriteError_GenericError_NotHandled(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()
	r, _ := newNodeReconcilerForHelperTest(t, node, nil)

	_, handled, err := r.handlePermanentWriteError(ctx, node, errors.New("network blip"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for generic error")
	}
}

func TestHandlePermanentWriteError_TruncatesLongMessage(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()
	r, _ := newNodeReconcilerForHelperTest(t, node, nil)

	longMsg := strings.Repeat("x", replicaFailureMessageMaxLen+500)

	_, handled, err := r.handlePermanentWriteError(ctx, node, invalidErr(longMsg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	deg := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil {
		t.Fatal("Degraded condition not set")
	}
	if !strings.Contains(deg.Message, "(truncated") {
		t.Errorf("expected truncation indicator in Degraded message; got tail: %q", tail(deg.Message, 80))
	}
}

// Regression test for the self-trigger defense (Layer 2). The helper must
// detect when conditions are already at the desired state and skip both the
// Status().Update and the Event emission — otherwise the operator would
// inflate the Event count and rely on apiserver no-op elision to break the
// reconcile loop.
func TestHandlePermanentWriteError_NoChange_SkipsWrite(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()

	// Pre-populate the conditions to exactly what the helper would produce
	// for this error, so the second call finds nothing to change.
	preErr := invalidErr("same message")
	preMsg := truncateReplicaFailureMessage(preErr.Error())
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            "Deployment rejected by apiserver: " + preMsg,
		ObservedGeneration: 7,
	})
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            "Deployment is invalid; see Degraded condition",
		ObservedGeneration: 7,
	})
	node.Status.ObservedGeneration = 7

	// Status updater would error if invoked — proves the helper does not call it.
	r, rec := newNodeReconcilerForHelperTest(t, node, errors.New("status update must not be called"))

	_, handled, err := r.handlePermanentWriteError(ctx, node, preErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event on no-change, got: %s", ev)
	default:
	}
}

func TestHandlePermanentWriteError_StatusConflict_RequeueTrue(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()

	statusConflictErr := apierrors.NewConflict(schema.GroupResource{Group: "curity.io", Resource: "identityservernodes"}, "rt-1", errors.New("status conflict"))

	r, rec := newNodeReconcilerForHelperTest(t, node, statusConflictErr)

	res, handled, err := r.handlePermanentWriteError(ctx, node, invalidErr("anything"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Errorf("expected Requeue:true result, got %+v", res)
	}

	// No event should fire on the conflict path — Status().Update failed.
	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event when status update conflicted, got: %s", ev)
	default:
	}
}

func TestHandlePermanentWriteError_StatusNonConflictError_Bubbles(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()

	apiDownErr := errors.New("connection refused")
	r, _ := newNodeReconcilerForHelperTest(t, node, apiDownErr)

	_, handled, err := r.handlePermanentWriteError(ctx, node, invalidErr("anything"))
	if !handled {
		t.Fatal("expected handled=true even on bubbled error")
	}
	if err == nil {
		t.Fatal("expected wrapped status error to bubble")
	}
	if !errors.Is(err, apiDownErr) {
		t.Errorf("expected wrapped %v; got: %v", apiDownErr, err)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ============================================================================
// extractInvalid direct tests (PR-1: shared extraction helper)
// ============================================================================

// fakeAPIStatus implements the apierrors.APIStatus interface but is NOT a
// *apierrors.StatusError. apierrors.IsInvalid returns true on this type
// (it uses errors.As against the interface, not the concrete type), but
// extractInvalid's errors.As against *apierrors.StatusError fails.
// Documents the defensive ok=false branch in extractInvalid.
type fakeAPIStatus struct {
	status metav1.Status
}

func (f *fakeAPIStatus) Error() string         { return f.status.Message }
func (f *fakeAPIStatus) Status() metav1.Status { return f.status }

func TestExtractInvalid_StandardInvalid(t *testing.T) {
	err := invalidErr("bad label syntax")
	kind, msg, ok := extractInvalid(err)
	if !ok {
		t.Fatal("expected ok=true on standard NewInvalid")
	}
	if kind != "Deployment" {
		t.Errorf("kind = %q, want Deployment", kind)
	}
	if msg == "" {
		t.Error("msg unexpectedly empty")
	}
}

func TestExtractInvalid_Nil(t *testing.T) {
	kind, msg, ok := extractInvalid(nil)
	if ok || kind != "" || msg != "" {
		t.Errorf("expected (\"\", \"\", false) for nil; got (%q, %q, %v)", kind, msg, ok)
	}
}

func TestExtractInvalid_GenericError(t *testing.T) {
	_, _, ok := extractInvalid(errors.New("network blip"))
	if ok {
		t.Error("expected ok=false for generic error")
	}
}

func TestExtractInvalid_WrongAPIErrorClass(t *testing.T) {
	cases := []error{
		apierrors.NewNotFound(schema.GroupResource{Resource: "deployments"}, "x"),
		apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "x", errors.New("rbac")),
		apierrors.NewConflict(schema.GroupResource{Resource: "deployments"}, "x", errors.New("modified")),
		apierrors.NewBadRequest("malformed"),
	}
	for _, e := range cases {
		if _, _, ok := extractInvalid(e); ok {
			t.Errorf("expected ok=false for %T", e)
		}
	}
}

func TestExtractInvalid_FmtErrorfWrap(t *testing.T) {
	raw := invalidErr("inner error")
	wrapped := fmt.Errorf("operator wrap: %w", raw)
	kind, msg, ok := extractInvalid(wrapped)
	if !ok {
		t.Fatal("expected errors.As to unwrap %w into *StatusError")
	}
	if kind != "Deployment" {
		t.Errorf("kind = %q, want Deployment", kind)
	}
	// The wrap text must NOT leak into the message — extractInvalid pulls
	// from statusErr.ErrStatus.Message directly, not from writeErr.Error().
	if strings.Contains(msg, "operator wrap:") {
		t.Errorf("operator-wrap text leaked into apiMsg: %q", msg)
	}
}

func TestExtractInvalid_CustomAPIStatusWrap_DefensiveBranch(t *testing.T) {
	// Construct an APIStatus implementer that is NOT *apierrors.StatusError.
	// Exercises the defensive errors.As-failure branch in extractInvalid.
	// apierrors.IsInvalid returns true (it uses APIStatus interface via
	// errors.As), but extractInvalid's errors.As against *StatusError fails.
	custom := &fakeAPIStatus{
		status: metav1.Status{
			Reason:  metav1.StatusReasonInvalid,
			Message: "custom message",
		},
	}
	if !apierrors.IsInvalid(custom) {
		t.Fatal("test setup: fakeAPIStatus should pass IsInvalid via APIStatus interface")
	}
	_, _, ok := extractInvalid(custom)
	if ok {
		t.Error("expected ok=false when errors.As(*StatusError) fails — defensive branch")
	}
}

func TestExtractInvalid_EmptyDetailsKind_FallbackToResource(t *testing.T) {
	// Build a NewInvalid with empty Group/Kind to verify the fallback.
	gk := schema.GroupKind{}
	err := apierrors.NewInvalid(gk, "x", field.ErrorList{
		field.Invalid(field.NewPath("metadata", "name"), "x", "invalid"),
	})
	kind, _, ok := extractInvalid(err)
	if !ok {
		t.Fatal("expected ok=true")
	}
	// With empty GroupKind, Details.Kind is also empty; extractInvalid
	// falls back to the literal "Resource" so condition messages remain
	// readable instead of saying " rejected by apiserver:".
	if kind != "Resource" {
		t.Errorf("expected fallback kind=\"Resource\" when Details.Kind is empty; got %q", kind)
	}
}

// ============================================================================
// Cross-path precedence (PR-1: helper Ready wins over applyReadyOverlay Ready)
// ============================================================================

// When a node already has PackagesReady=False/PackageFetchFailed set (e.g. by
// the pod-watch translator from a prior reconcile) AND Ready=False with the
// PackagesNotReady reason from applyReadyOverlay, a subsequent IsInvalid on
// a child write must update Ready to False/InvalidSpec without disturbing
// PackagesReady. Documents U-7's verified non-fight: the helper bails
// before reaching the second applyReadyOverlay call site, so InvalidSpec is
// the last writer for Ready in any reconcile that hits both.
func TestHandlePermanentWriteError_PrecedenceOverPackagesNotReady(t *testing.T) {
	ctx := context.Background()
	node := freshTestNode()

	// Pre-set state: prior reconcile flagged PackagesReady=False, the overlay
	// then forced Ready=False/PackagesNotReady. This is the steady-state
	// shape on pkg-smoke today (per project_packages_status_visibility_followup).
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionPackagesReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonPackageFetchFailed,
		Message:            "init container exited 6",
		ObservedGeneration: 7,
	})
	apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonPackagesNotReady,
		Message:            "package fetch not ready; see PackagesReady condition",
		ObservedGeneration: 7,
	})

	r, _ := newNodeReconcilerForHelperTest(t, node, nil)

	// Now a child write returns IsInvalid (e.g. user simultaneously set bad
	// podLabels — same reconcile sees both signals).
	_, handled, err := r.handlePermanentWriteError(ctx, node, invalidErr("bad label syntax"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	ready := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Ready reason should be InvalidSpec (helper wins); got %+v", ready)
	}

	// PackagesReady must remain untouched — the helper has no business
	// rewriting a condition it doesn't own. Future debugging relies on this.
	pkgs := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionPackagesReady)
	if pkgs == nil || pkgs.Reason != v1alpha1.ReasonPackageFetchFailed {
		t.Errorf("PackagesReady should stay untouched by helper; got %+v", pkgs)
	}

	deg := apimeta.FindStatusCondition(node.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil || deg.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Degraded should be InvalidSpec; got %+v", deg)
	}
}

// ============================================================================
// Cluster-side helper tests
// ============================================================================

func newClusterReconcilerForHelperTest(t *testing.T, cluster *v1alpha1.IdentityServerCluster, statusUpdateErr error) (*IdentityServerClusterReconciler, *record.FakeRecorder) {
	t.Helper()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("adding v1alpha1 to scheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(cluster).
		WithStatusSubresource(&v1alpha1.IdentityServerCluster{})

	if statusUpdateErr != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" {
					return statusUpdateErr
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		})
	}

	rec := record.NewFakeRecorder(8)
	return &IdentityServerClusterReconciler{
		Client:   builder.Build(),
		Scheme:   s,
		Recorder: rec,
	}, rec
}

func freshTestCluster() *v1alpha1.IdentityServerCluster {
	return &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "c-1",
			Namespace:  "ns",
			Generation: 5,
		},
	}
}

// invalidErrForKind builds an apierrors.NewInvalid for an arbitrary Kind,
// so cluster-side tests can simulate Secret rejection (admin creds path)
// AND Job rejection (genclust path) from the same factory.
func invalidErrForKind(kind, name, message string) error {
	gk := schema.GroupKind{Kind: kind}
	return apierrors.NewInvalid(gk, name, field.ErrorList{
		field.Invalid(field.NewPath("metadata", "name"), name, message),
	})
}

// T-1.
func TestClusterHandlePermanentWriteError_IsInvalid_FlipsDegradedAndReady(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, rec := newClusterReconcilerForHelperTest(t, cluster, nil)

	res, handled, err := r.handlePermanentWriteError(ctx, cluster,
		invalidErrForKind("Secret", "bad..secret..name", "must be DNS-1123 subdomain"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected zero Result, got %+v", res)
	}

	deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil || deg.Status != metav1.ConditionTrue || deg.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Degraded condition wrong: %+v", deg)
	}
	if deg != nil && !strings.Contains(deg.Message, "Secret rejected by apiserver:") {
		t.Errorf("Degraded message lacks expected prefix: %q", deg.Message)
	}
	if deg != nil && strings.Contains(deg.Message, "failed to create") {
		t.Errorf("Degraded message contains operator-wrap text: %q", deg.Message)
	}
	if deg != nil && deg.ObservedGeneration != 5 {
		t.Errorf("Degraded ObservedGeneration = %d, want 5", deg.ObservedGeneration)
	}

	// End-to-end Ready: cluster reports Ready=False when the spec it just
	// received cannot be honored. Matches the packages applyReadyOverlay
	// design intent — Ready means "desired spec is being honored", not
	// "some old pods are still serving."
	ready := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != v1alpha1.ReasonInvalidSpec {
		t.Errorf("Ready condition wrong: %+v", ready)
	}
	if ready != nil && !strings.Contains(ready.Message, "see Degraded condition") {
		t.Errorf("Ready message should delegate to Degraded; got: %q", ready.Message)
	}

	// Other operational conditions stay observed (helper does not synthesize
	// Available / Progressing / ClusterConfigReady from a spec rejection).
	for _, condType := range []string{v1alpha1.ConditionAvailable, v1alpha1.ConditionProgressing, v1alpha1.ConditionClusterConfigReady} {
		if c := apimeta.FindStatusCondition(cluster.Status.Conditions, condType); c != nil {
			t.Errorf("helper must not write %s, got: %+v", condType, c)
		}
	}

	if cluster.Status.ObservedGeneration != 5 {
		t.Errorf("top-level ObservedGeneration = %d, want 5", cluster.Status.ObservedGeneration)
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning "+v1alpha1.ReasonInvalidSpec) {
			t.Errorf("unexpected event: %q", ev)
		}
	default:
		t.Error("expected one Warning event, got none")
	}
}

// T-2. Regression test for the operator-wrap unwrap behavior.
func TestClusterHandlePermanentWriteError_IsInvalid_FromWrappedError(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	raw := invalidErrForKind("Secret", "bad..secret..name", "must be DNS-1123 subdomain")
	wrapped := fmt.Errorf("failed to create admin credentials secret: %w", raw)

	_, handled, err := r.handlePermanentWriteError(ctx, cluster, wrapped)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true (IsInvalid unwraps via %w)")
	}

	deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil {
		t.Fatal("Degraded not set")
	}
	if strings.Contains(deg.Message, "failed to create admin credentials secret") {
		t.Errorf("operator wrap leaked into condition message: %q", deg.Message)
	}
	if !strings.Contains(deg.Message, "bad..secret..name") {
		t.Errorf("apiserver detail missing from message: %q", deg.Message)
	}
}

// T-3.
func TestClusterHandlePermanentWriteError_IsConflict_NotHandled(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	conflictErr := apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "x", errors.New("the object has been modified"))

	_, handled, err := r.handlePermanentWriteError(ctx, cluster, conflictErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for IsConflict")
	}
	if len(cluster.Status.Conditions) != 0 {
		t.Errorf("expected no conditions touched, got: %+v", cluster.Status.Conditions)
	}
}

// T-4.
func TestClusterHandlePermanentWriteError_IsNotFound_NotHandled(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	nfErr := apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "x")

	_, handled, err := r.handlePermanentWriteError(ctx, cluster, nfErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for IsNotFound")
	}
}

// T-5.
func TestClusterHandlePermanentWriteError_GenericError_NotHandled(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	_, handled, err := r.handlePermanentWriteError(ctx, cluster, errors.New("network blip"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("expected handled=false for generic error")
	}
}

// T-6.
func TestClusterHandlePermanentWriteError_TruncatesLongMessage(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	longMsg := strings.Repeat("x", replicaFailureMessageMaxLen+500)

	_, handled, err := r.handlePermanentWriteError(ctx, cluster,
		invalidErrForKind("Secret", "n", longMsg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil {
		t.Fatal("Degraded not set")
	}
	if !strings.Contains(deg.Message, "(truncated") {
		t.Errorf("expected truncation indicator; got tail: %q", tail(deg.Message, 80))
	}
}

// T-7. Regression test for the Layer-2 self-trigger defense.
func TestClusterHandlePermanentWriteError_NoChange_SkipsWrite(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()

	preErr := invalidErrForKind("Secret", "bad..secret..name", "must be DNS-1123 subdomain")
	var preStatusErr *apierrors.StatusError
	if !errors.As(preErr, &preStatusErr) {
		t.Fatal("test setup: errors.As on NewInvalid output should succeed")
	}
	preApiMsg := truncateReplicaFailureMessage(preStatusErr.ErrStatus.Message)
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            "Secret rejected by apiserver: " + preApiMsg,
		ObservedGeneration: 5,
	})
	// Helper also writes Ready=False/InvalidSpec; pre-populate so the
	// changed-bool gate sees nothing to update.
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            "Secret is invalid; see Degraded condition",
		ObservedGeneration: 5,
	})
	cluster.Status.ObservedGeneration = 5

	// Status updater would error if invoked — proves the helper skips the call.
	r, rec := newClusterReconcilerForHelperTest(t, cluster, errors.New("status update must not be called"))

	_, handled, err := r.handlePermanentWriteError(ctx, cluster, preErr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event on no-change, got: %s", ev)
	default:
	}
}

// T-8.
func TestClusterHandlePermanentWriteError_StatusConflict_RequeueTrue(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()

	statusConflictErr := apierrors.NewConflict(schema.GroupResource{Group: "curity.io", Resource: "identityserverclusters"}, "c-1", errors.New("status conflict"))

	r, rec := newClusterReconcilerForHelperTest(t, cluster, statusConflictErr)

	res, handled, err := r.handlePermanentWriteError(ctx, cluster,
		invalidErrForKind("Secret", "bad..secret..name", "anything"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Errorf("expected Requeue:true, got %+v", res)
	}

	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event when status update conflicted, got: %s", ev)
	default:
	}
}

// T-9.
func TestClusterHandlePermanentWriteError_StatusNonConflictError_Bubbles(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()

	apiDownErr := errors.New("connection refused")
	r, _ := newClusterReconcilerForHelperTest(t, cluster, apiDownErr)

	_, handled, err := r.handlePermanentWriteError(ctx, cluster,
		invalidErrForKind("Secret", "n", "anything"))
	if !handled {
		t.Fatal("expected handled=true even on bubbled error")
	}
	if err == nil {
		t.Fatal("expected wrapped status error to bubble")
	}
	if !errors.Is(err, apiDownErr) {
		t.Errorf("expected wrapped %v; got: %v", apiDownErr, err)
	}
}

// T-10. Kind comes from the structured error, not a hardcoded string.
func TestClusterHandlePermanentWriteError_KindFromJob(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	_, handled, err := r.handlePermanentWriteError(ctx, cluster,
		invalidErrForKind("Job", "long-cluster-name-genclust", "invalid"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}

	deg := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionDegraded)
	if deg == nil {
		t.Fatal("Degraded not set")
	}
	if !strings.Contains(deg.Message, "Job rejected by apiserver:") {
		t.Errorf("Kind label is wrong; want 'Job', got message: %q", deg.Message)
	}
	if strings.Contains(deg.Message, "Secret rejected by apiserver:") {
		t.Errorf("Kind label leaked from another test; got: %q", deg.Message)
	}
}

// ============================================================================
// U7 - handleAdmissionForbidden (Layer 1)
// ============================================================================

// forbiddenErrForPod builds an apierrors.NewForbidden modeling SCC/PSA/quota
// denial from apiserver dry-run admission. The message field is what the
// helper truncates into the condition.
func forbiddenErrForPod(podName, msg string) error {
	gr := schema.GroupResource{Group: "", Resource: "pods"}
	return apierrors.NewForbidden(gr, podName, errors.New(msg))
}

func TestHandleAdmissionForbidden_IsForbidden_SetsCondition(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, rec := newClusterReconcilerForHelperTest(t, cluster, nil)

	res, handled, err := r.handleAdmissionForbidden(ctx, cluster,
		forbiddenErrForPod("pss-cluster-config-job-dryrun-preflight",
			"violates PodSecurity \"restricted:latest\": ..."))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res.RequeueAfter != 5*time.Minute {
		t.Errorf("expected RequeueAfter=5m, got %v", res.RequeueAfter)
	}

	c := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "JobPodAdmissionForbidden" {
		t.Errorf("ClusterConfigReady wrong: %+v", c)
	}
	if c != nil && !strings.Contains(c.Message, "genclust pod admission denied:") {
		t.Errorf("message lacks prefix: %q", c.Message)
	}
	if c != nil && !strings.Contains(c.Message, "violates PodSecurity") {
		t.Errorf("message lacks apiserver text: %q", c.Message)
	}
	if c != nil && c.ObservedGeneration != 5 {
		t.Errorf("ObservedGeneration=%d want=5", c.ObservedGeneration)
	}

	// Operational conditions must NOT be touched.
	for _, condType := range []string{v1alpha1.ConditionReady, v1alpha1.ConditionAvailable, v1alpha1.ConditionDegraded} {
		if got := apimeta.FindStatusCondition(cluster.Status.Conditions, condType); got != nil {
			t.Errorf("helper must not write %s, got: %+v", condType, got)
		}
	}

	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "Warning JobPodAdmissionForbidden") {
			t.Errorf("unexpected event: %q", ev)
		}
	default:
		t.Error("expected one Warning event, got none")
	}
}

func TestHandleAdmissionForbidden_IsInvalid_NotHandled(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	res, handled, err := r.handleAdmissionForbidden(ctx, cluster,
		invalidErrForKind("Pod", "n", "invalid"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("IsInvalid must NOT be handled by handleAdmissionForbidden (handlePermanentWriteError covers it)")
	}
	if res != (ctrl.Result{}) {
		t.Errorf("expected zero Result, got %+v", res)
	}
}

func TestHandleAdmissionForbidden_GenericError_NotHandled(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	_, handled, err := r.handleAdmissionForbidden(ctx, cluster, errors.New("network down"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Error("plain error must NOT be handled")
	}
}

func TestHandleAdmissionForbidden_NoChange_SkipsWrite(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()

	// Pre-seed the condition with the message the helper would produce for our
	// fixture error → SetStatusCondition will return changed=false.
	apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionClusterConfigReady,
		Status:             metav1.ConditionFalse,
		Reason:             "JobPodAdmissionForbidden",
		Message:            `genclust pod admission denied: pods "p" is forbidden: SCC denied`,
		ObservedGeneration: 5,
	})
	cluster.Status.ObservedGeneration = 5

	// Status updater errors loudly if called.
	r, rec := newClusterReconcilerForHelperTest(t, cluster, errors.New("status update must not be called"))

	res, handled, err := r.handleAdmissionForbidden(ctx, cluster, forbiddenErrForPod("p", "SCC denied"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	// Requeue must STILL fire on no-change so subsequent reconciles keep checking.
	if res.RequeueAfter != 5*time.Minute {
		t.Errorf("no-change path must still return 5m requeue, got %v", res.RequeueAfter)
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event on no-change, got: %s", ev)
	default:
	}
}

func TestHandleAdmissionForbidden_StatusConflict_RequeueTrue(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()

	statusConflictErr := apierrors.NewConflict(
		schema.GroupResource{Group: "curity.io", Resource: "identityserverclusters"},
		"c-1", errors.New("status conflict"))

	r, rec := newClusterReconcilerForHelperTest(t, cluster, statusConflictErr)

	res, handled, err := r.handleAdmissionForbidden(ctx, cluster,
		forbiddenErrForPod("p", "SCC denied"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Errorf("expected Requeue:true, got %+v", res)
	}
	select {
	case ev := <-rec.Events:
		t.Errorf("expected no event when status conflicted, got: %s", ev)
	default:
	}
}

func TestHandleAdmissionForbidden_TruncatesLongMessage(t *testing.T) {
	ctx := context.Background()
	cluster := freshTestCluster()
	r, _ := newClusterReconcilerForHelperTest(t, cluster, nil)

	longMsg := strings.Repeat("x", 1000)
	_, handled, err := r.handleAdmissionForbidden(ctx, cluster, forbiddenErrForPod("p", longMsg))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true")
	}
	c := apimeta.FindStatusCondition(cluster.Status.Conditions, v1alpha1.ConditionClusterConfigReady)
	if c == nil {
		t.Fatal("condition not set")
	}
	// The truncation helper used by both classifiers caps at ~256 chars; we
	// just check the message is bounded to keep apiserver responses sane.
	if len(c.Message) > 4096 {
		t.Errorf("message length %d exceeds reasonable cap", len(c.Message))
	}
}

// ============================================================================
// U8 - dryRunPodAdmission shape
// ============================================================================

func TestDryRunPodAdmission_PodShapeMirrorsJobTemplate(t *testing.T) {
	// Verify that the helper constructs a Pod with the deterministic name
	// (NOT GenerateName — that was the smoke-test bug) and inherits the Job's
	// pod-template labels/annotations and Spec. We use the fake client to
	// intercept the Create call rather than actually hitting an apiserver;
	// the apiserver's admission chain is exercised in envtest/E2E.
	ctx := context.Background()
	s := newScheme(t)
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	cluster := &v1alpha1.IdentityServerCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns"},
	}

	var captured *corev1.Pod
	cli := fake.NewClientBuilder().
		WithScheme(s).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if pod, ok := obj.(*corev1.Pod); ok {
					captured = pod.DeepCopy()
				}
				// Return success — the helper doesn't care since it just bubbles err.
				return nil
			},
		}).
		Build()

	r := &IdentityServerClusterReconciler{Client: cli, Scheme: s}
	job := buildClusterConfigJob(cluster, "admin", "config-hash-1", portConfig)

	if err := r.dryRunPodAdmission(ctx, cluster, job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if captured == nil {
		t.Fatal("Create was not invoked")
	}

	// Deterministic name (smoke-test fix).
	wantName := job.Name + "-dryrun-preflight"
	if captured.Name != wantName {
		t.Errorf("Pod name = %q, want %q", captured.Name, wantName)
	}
	if captured.GenerateName != "" {
		t.Errorf("GenerateName must be empty (deterministic name only), got %q", captured.GenerateName)
	}
	if captured.Namespace != cluster.Namespace {
		t.Errorf("Pod namespace = %q, want %q", captured.Namespace, cluster.Namespace)
	}
	// Labels inherited from Job pod template.
	for k, v := range job.Spec.Template.Labels {
		if captured.Labels[k] != v {
			t.Errorf("label %q = %q, want %q", k, captured.Labels[k], v)
		}
	}
	// Spec mirrors the Job pod template.
	if captured.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", captured.Spec.RestartPolicy)
	}
	if len(captured.Spec.Containers) == 0 || captured.Spec.Containers[0].Name != "genclust" {
		t.Errorf("expected genclust container, got %+v", captured.Spec.Containers)
	}
}

// jobPodFor builds a Pod whose Spec matches what the cluster-config Job
// template stamps. Test cases override status fields as needed.
func jobPodFor(name string, created time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "ns",
			Labels:            map[string]string{"curity.io/component": "cluster-config"},
			CreationTimestamp: metav1.NewTime(created),
		},
	}
}

// withWaitingGenclust returns a copy of pod with a genclust container in the
// given Waiting state.
func withWaitingGenclust(pod corev1.Pod, reason, message string) corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "genclust",
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  reason,
				Message: message,
			},
		},
	}}
	return pod
}

// withTerminatedGenclust returns a copy of pod with a genclust container in
// the given Terminated state.
func withTerminatedGenclust(pod corev1.Pod, exitCode int32) corev1.Pod {
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "genclust",
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode},
		},
	}}
	return pod
}

// withPodScheduledUnschedulable returns a copy of pod marked Phase=Pending
// with PodScheduled=False/Unschedulable.
func withPodScheduledUnschedulable(pod corev1.Pod, msg string) corev1.Pod {
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:    corev1.PodScheduled,
		Status:  corev1.ConditionFalse,
		Reason:  corev1.PodReasonUnschedulable,
		Message: msg,
	}}
	return pod
}

func TestTranslateJobPodAdmission(t *testing.T) {
	t0 := time.Now()

	cases := []struct {
		name            string
		pods            []corev1.Pod
		failedCreateMsg string
		wantSet         bool
		wantReason      string
		wantInMessage   string
	}{
		{
			name:    "no pods, no event => set=false",
			pods:    nil,
			wantSet: false,
		},
		{
			name:            "no pods + FailedCreate event => JobPodAdmissionFailed",
			failedCreateMsg: "SCC denied",
			wantSet:         true,
			wantReason:      "JobPodAdmissionFailed",
			wantInMessage:   "SCC denied",
		},
		{
			name: "pod Pending + Unschedulable => JobPodSchedulingFailed",
			pods: []corev1.Pod{
				withPodScheduledUnschedulable(jobPodFor("p", t0), "0/3 nodes match"),
			},
			wantSet:       true,
			wantReason:    "JobPodSchedulingFailed",
			wantInMessage: "0/3 nodes match",
		},
		{
			name: "pod ImagePullBackOff => JobPodImagePullFailed",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "ImagePullBackOff", "Back-off"),
			},
			wantSet:       true,
			wantReason:    "JobPodImagePullFailed",
			wantInMessage: "Back-off",
		},
		{
			name: "pod ErrImagePull => JobPodImagePullFailed",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "ErrImagePull", "manifest unknown"),
			},
			wantSet:       true,
			wantReason:    "JobPodImagePullFailed",
			wantInMessage: "manifest unknown",
		},
		{
			name: "pod InvalidImageName => JobPodImagePullFailed",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "InvalidImageName", "bad ref"),
			},
			wantSet:    true,
			wantReason: "JobPodImagePullFailed",
		},
		{
			name: "pod CreateContainerConfigError => JobPodCreateConfigError",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "CreateContainerConfigError", "secret \"x\" not found"),
			},
			wantSet:       true,
			wantReason:    "JobPodCreateConfigError",
			wantInMessage: "secret",
		},
		{
			name: "pod Waiting=PodInitializing => transient, set=false",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "PodInitializing", ""),
			},
			wantSet: false,
		},
		{
			name: "pod Waiting=ContainerCreating => transient, set=false",
			pods: []corev1.Pod{
				withWaitingGenclust(jobPodFor("p", t0), "ContainerCreating", ""),
			},
			wantSet: false,
		},
		{
			name: "pod Phase=Running, no waiting => set=false",
			pods: []corev1.Pod{
				func() corev1.Pod {
					p := jobPodFor("p", t0)
					p.Status.Phase = corev1.PodRunning
					return p
				}(),
			},
			wantSet: false,
		},
		{
			name: "pod Phase=Failed exitCode=1 => set=false (existing JobFailed branch handles post-BackoffLimit)",
			pods: []corev1.Pod{
				withTerminatedGenclust(jobPodFor("p", t0), 1),
			},
			wantSet: false,
		},
		{
			name: "pod DeletionTimestamp set => ignored, set=false",
			pods: []corev1.Pod{
				func() corev1.Pod {
					p := withWaitingGenclust(jobPodFor("p", t0), "ImagePullBackOff", "Back-off")
					now := metav1.Now()
					p.DeletionTimestamp = &now
					return p
				}(),
			},
			wantSet: false,
		},
		{
			name: "2 pods, newest Running, oldest Failed => set=false (newest wins)",
			pods: []corev1.Pod{
				withTerminatedGenclust(jobPodFor("old", t0), 1),
				func() corev1.Pod {
					p := jobPodFor("new", t0.Add(2*time.Second))
					p.Status.Phase = corev1.PodRunning
					return p
				}(),
			},
			wantSet: false,
		},
		{
			name: "pod Running AND failedCreateMsg present (stale event) => set=false, Layer 3 wins",
			pods: []corev1.Pod{
				func() corev1.Pod {
					p := jobPodFor("p", t0)
					p.Status.Phase = corev1.PodRunning
					return p
				}(),
			},
			failedCreateMsg: "stale SCC denial",
			wantSet:         false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := translateJobPodAdmission(tc.pods, tc.failedCreateMsg)
			if res.set != tc.wantSet {
				t.Fatalf("set=%v want=%v (full res=%+v)", res.set, tc.wantSet, res)
			}
			if !tc.wantSet {
				return
			}
			if res.reason != tc.wantReason {
				t.Errorf("reason=%q want=%q", res.reason, tc.wantReason)
			}
			if res.status != metav1.ConditionFalse {
				t.Errorf("status=%v want=False", res.status)
			}
			if tc.wantInMessage != "" && !strings.Contains(res.message, tc.wantInMessage) {
				t.Errorf("message=%q does not contain %q", res.message, tc.wantInMessage)
			}
		})
	}
}

// mostRecentClusterConfigPod tie-break: identical CreationTimestamp resolves
// deterministically by Name lexical order.
func TestMostRecentClusterConfigPod_TieBreakByName(t *testing.T) {
	t0 := time.Now()
	pods := []corev1.Pod{
		jobPodFor("b", t0),
		jobPodFor("a", t0),
	}
	got := mostRecentClusterConfigPod(pods)
	if got == nil || got.Name != "a" {
		t.Fatalf("tie should resolve to lexical-first 'a', got %+v", got)
	}
}

func TestMostRecentClusterConfigPod_Empty(t *testing.T) {
	if got := mostRecentClusterConfigPod(nil); got != nil {
		t.Fatalf("empty input must return nil, got %+v", got)
	}
}

// ============================================================================
// findClusterForJobPod (mapFunc Pod → cluster CR)
// ============================================================================

func TestFindClusterForJobPod(t *testing.T) {
	r := &IdentityServerClusterReconciler{}
	ctx := context.Background()

	t.Run("nil object returns empty", func(t *testing.T) {
		if got := r.findClusterForJobPod(ctx, nil); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("wrong type returns empty", func(t *testing.T) {
		if got := r.findClusterForJobPod(ctx, &batchv1.Job{}); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("missing cluster label returns empty", func(t *testing.T) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns"}}
		if got := r.findClusterForJobPod(ctx, pod); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("labeled pod returns request", func(t *testing.T) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "p",
				Namespace: "ns",
				Labels:    map[string]string{"curity.io/cluster": "alpha"},
			},
		}
		got := r.findClusterForJobPod(ctx, pod)
		if len(got) != 1 || got[0].Name != "alpha" || got[0].Namespace != "ns" {
			t.Errorf("unexpected requests: %+v", got)
		}
	})
}

// ============================================================================
// findClusterForJobFailedCreateEvent (mapFunc Event → cluster CR)
// ============================================================================

func TestFindClusterForJobFailedCreateEvent(t *testing.T) {
	ctx := context.Background()
	s := schemeWithBatch(t)

	t.Run("non-event object returns empty", func(t *testing.T) {
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).Build(),
		}
		if got := r.findClusterForJobFailedCreateEvent(ctx, &batchv1.Job{}); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("job not in cache returns empty", func(t *testing.T) {
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).Build(),
		}
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{
				Kind: "Job", Name: "ghost", Namespace: "ns",
			},
		}
		if got := r.findClusterForJobFailedCreateEvent(ctx, ev); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("job without cluster label returns empty", func(t *testing.T) {
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "ns"}}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(job).Build(),
		}
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{Kind: "Job", Name: "j", Namespace: "ns"},
		}
		if got := r.findClusterForJobFailedCreateEvent(ctx, ev); len(got) != 0 {
			t.Errorf("expected empty, got %+v", got)
		}
	})

	t.Run("job with cluster label returns one request", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alpha-cluster-config-job",
				Namespace: "ns",
				Labels:    map[string]string{"curity.io/cluster": "alpha"},
			},
		}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(job).Build(),
		}
		ev := &corev1.Event{
			InvolvedObject: corev1.ObjectReference{
				Kind: "Job", Name: "alpha-cluster-config-job", Namespace: "ns",
			},
		}
		got := r.findClusterForJobFailedCreateEvent(ctx, ev)
		if len(got) != 1 || got[0].Name != "alpha" || got[0].Namespace != "ns" {
			t.Errorf("unexpected requests: %+v", got)
		}
	})
}

// ============================================================================
// latestJobFailedCreateMessage (Layer 2 cache-read helper)
// ============================================================================

func TestLatestJobFailedCreateMessage(t *testing.T) {
	ctx := context.Background()
	s := schemeWithBatch(t)
	const ns = "ns"
	jobUID := types.UID("job-uid-1")
	otherUID := types.UID("job-uid-2")

	mkEvent := func(name, msg string, uid types.UID, ts metav1.Time) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: ns},
			InvolvedObject: corev1.ObjectReference{Kind: "Job", UID: uid, Namespace: ns},
			Reason:         "FailedCreate",
			Type:           corev1.EventTypeWarning,
			Message:        msg,
			LastTimestamp:  ts,
		}
	}

	t0 := metav1.Now()
	tLater := metav1.NewTime(t0.Add(10))

	t.Run("empty cache returns empty string", func(t *testing.T) {
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).Build(),
		}
		if got := r.latestJobFailedCreateMessage(ctx, jobUID, ns); got != "" {
			t.Errorf("empty cache must return \"\", got %q", got)
		}
	})

	t.Run("no UID match returns empty string", func(t *testing.T) {
		objs := []client.Object{mkEvent("e1", "other-job msg", otherUID, t0)}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		}
		if got := r.latestJobFailedCreateMessage(ctx, jobUID, ns); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("exact UID match returns message", func(t *testing.T) {
		objs := []client.Object{mkEvent("e1", "SCC denied", jobUID, t0)}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		}
		got := r.latestJobFailedCreateMessage(ctx, jobUID, ns)
		if !strings.Contains(got, "SCC denied") {
			t.Errorf("expected \"SCC denied\" in result, got %q", got)
		}
	})

	t.Run("multiple matches, newest wins", func(t *testing.T) {
		objs := []client.Object{
			mkEvent("e-old", "old msg", jobUID, t0),
			mkEvent("e-new", "new msg", jobUID, tLater),
		}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		}
		got := r.latestJobFailedCreateMessage(ctx, jobUID, ns)
		if !strings.Contains(got, "new msg") {
			t.Errorf("expected newest \"new msg\", got %q", got)
		}
		if strings.Contains(got, "old msg") {
			t.Errorf("expected NOT to contain \"old msg\", got %q", got)
		}
	})

	t.Run("other UID events are ignored", func(t *testing.T) {
		objs := []client.Object{
			mkEvent("e-other", "other-job msg", otherUID, tLater),
			mkEvent("e-target", "target msg", jobUID, t0),
		}
		r := &IdentityServerClusterReconciler{
			Client: fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build(),
		}
		got := r.latestJobFailedCreateMessage(ctx, jobUID, ns)
		if !strings.Contains(got, "target msg") {
			t.Errorf("expected target msg, got %q", got)
		}
		if strings.Contains(got, "other-job msg") {
			t.Errorf("must NOT contain other-job msg, got %q", got)
		}
	})
}
