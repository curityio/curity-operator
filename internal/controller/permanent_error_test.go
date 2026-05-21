package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

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

	res, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", invalidErr("invalid label syntax"))
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", conflictErr)
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", nfErr)
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", errors.New("network blip"))
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", invalidErr(longMsg))
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", preErr)
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

	res, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", invalidErr("anything"))
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

	_, handled, err := r.handlePermanentWriteError(ctx, node, "Deployment", invalidErr("anything"))
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
func TestClusterHandlePermanentWriteError_IsInvalid_FlipsDegraded(t *testing.T) {
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

	// Assert no operational conditions written.
	for _, condType := range []string{v1alpha1.ConditionReady, v1alpha1.ConditionAvailable, v1alpha1.ConditionProgressing, v1alpha1.ConditionClusterConfigReady} {
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
