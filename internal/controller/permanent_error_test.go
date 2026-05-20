package controller

import (
	"context"
	"errors"
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
