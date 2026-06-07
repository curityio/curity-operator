package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// errAdminCredsSecretMissing signals a user-provided adminCredentials Secret
// that does not exist, so the operator waits rather than fabricating one.
var errAdminCredsSecretMissing = errors.New("admin credentials Secret does not exist")

// isValidSecretName reports whether name is a valid DNS-1123 subdomain. An
// invalid name can never exist, so the wait path must not block on it.
func isValidSecretName(name string) bool {
	return len(validation.IsDNS1123Subdomain(name)) == 0
}

// extractInvalid returns the apiserver's Kind and Message from an IsInvalid
// write error, or (_, _, false) for anything else — including IsInvalid
// errors wrapped in a custom non-StatusError type (defensive against future
// refactors that might wrap apierrors differently).
//
// Kind falls back to the literal "Resource" when Details is missing — keeps
// condition messages readable even on apierror shapes that omit it.
func extractInvalid(writeErr error) (kind, apiMsg string, ok bool) {
	if !apierrors.IsInvalid(writeErr) {
		return "", "", false
	}
	var statusErr *apierrors.StatusError
	if !errors.As(writeErr, &statusErr) || statusErr == nil {
		return "", "", false
	}
	kind = "Resource"
	if statusErr.ErrStatus.Details != nil && statusErr.ErrStatus.Details.Kind != "" {
		kind = statusErr.ErrStatus.Details.Kind
	}
	return kind, truncateReplicaFailureMessage(statusErr.ErrStatus.Message), true
}

// handlePermanentWriteError classifies an apiserver IsInvalid write error
// as a permanent spec error and surfaces it on the node CR. Sets both
// Degraded=True/InvalidSpec and Ready=False/InvalidSpec — end-to-end Ready
// semantics: a spec the apiserver rejects is not being honored, even if
// older pods are still serving.
//
// Calls apimeta.SetStatusCondition directly so the changed-bool gate can
// skip Status().Update + Event when the conditions are already correct. The
// For() watch's GenerationChangedPredicate provides the Layer-1 self-trigger
// defense; this gate is Layer 2. If a future refactor pipes this through
// setCondition, restore the direct call so the gate stays effective.
func (r *IdentityServerNodeReconciler) handlePermanentWriteError(
	ctx context.Context,
	node *v1alpha1.IdentityServerNode,
	writeErr error,
) (ctrl.Result, bool, error) {
	kind, apiMsg, ok := extractInvalid(writeErr)
	if !ok {
		return ctrl.Result{}, false, nil
	}

	degChanged := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s rejected by apiserver: %s", kind, apiMsg),
		ObservedGeneration: node.Generation,
	})
	readyChanged := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s is invalid; see Degraded condition", kind),
		ObservedGeneration: node.Generation,
	})
	if !degChanged && !readyChanged {
		return ctrl.Result{}, true, nil
	}

	node.Status.ObservedGeneration = node.Generation

	if err := r.Status().Update(ctx, node); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("updating status after invalid %s: %w", kind, err)
	}

	r.Recorder.Eventf(node, corev1.EventTypeWarning, v1alpha1.ReasonInvalidSpec,
		"%s rejected by apiserver: %s", kind, apiMsg)

	return ctrl.Result{}, true, nil
}

// handlePermanentWriteError surfaces an apiserver IsInvalid write error on
// the cluster CR. Same shape as the node-side helper post-convergence:
// Degraded=True/InvalidSpec and Ready=False/InvalidSpec, one Warning event,
// changed-bool gated.
//
// Ready=False/InvalidSpec is a new (PR-1) behavior on the cluster side. It
// closes the stale-True window where a bad-spec edit on a previously-Ready
// cluster left Ready=True until something else cleared it. End-to-end Ready
// semantics: the new spec isn't being honored, surface that.
func (r *IdentityServerClusterReconciler) handlePermanentWriteError(
	ctx context.Context,
	cluster *v1alpha1.IdentityServerCluster,
	writeErr error,
) (ctrl.Result, bool, error) {
	kind, apiMsg, ok := extractInvalid(writeErr)
	if !ok {
		return ctrl.Result{}, false, nil
	}

	degChanged := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s rejected by apiserver: %s", kind, apiMsg),
		ObservedGeneration: cluster.Generation,
	})
	readyChanged := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s is invalid; see Degraded condition", kind),
		ObservedGeneration: cluster.Generation,
	})
	if !degChanged && !readyChanged {
		return ctrl.Result{}, true, nil
	}

	cluster.Status.ObservedGeneration = cluster.Generation

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("updating status after invalid %s: %w", kind, err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, v1alpha1.ReasonInvalidSpec,
		"%s rejected by apiserver: %s", kind, apiMsg)

	return ctrl.Result{}, true, nil
}

// handleAdmissionForbidden classifies an apiserver IsForbidden response from
// the cluster-config dry-run pod Create as a pod-admission denial (SCC,
// PodSecurity, ResourceQuota, validating webhook). Sets
// ClusterConfigReady=False/JobPodAdmissionForbidden and emits one Warning
// event on transition. Returns RequeueAfter=5m on both transition and no-op
// paths so the operator re-checks admission state periodically — no
// Pod-Create event exists to wake us when the user fixes the underlying
// problem.
func (r *IdentityServerClusterReconciler) handleAdmissionForbidden(
	ctx context.Context,
	cluster *v1alpha1.IdentityServerCluster,
	writeErr error,
) (ctrl.Result, bool, error) {
	if !apierrors.IsForbidden(writeErr) {
		return ctrl.Result{}, false, nil
	}

	var statusErr *apierrors.StatusError
	if !errors.As(writeErr, &statusErr) || statusErr == nil {
		return ctrl.Result{}, false, nil
	}

	apiMsg := truncateReplicaFailureMessage(statusErr.ErrStatus.Message)

	changed := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionClusterConfigReady,
		Status:             metav1.ConditionFalse,
		Reason:             "JobPodAdmissionForbidden",
		Message:            fmt.Sprintf("genclust pod admission denied: %s", apiMsg),
		ObservedGeneration: cluster.Generation,
	})

	if !changed {
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, true, nil
	}

	cluster.Status.ObservedGeneration = cluster.Generation

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("updating status after admission denied: %w", err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "JobPodAdmissionForbidden",
		"genclust pod admission denied: %s", apiMsg)

	return ctrl.Result{RequeueAfter: 5 * time.Minute}, true, nil
}

// handleAdminCredsSecretMissing surfaces a missing user-provided adminCredentials
// Secret as Degraded=True and Ready=False/AdminCredsSecretMissing plus one
// Warning, changed-bool gated. No RequeueAfter — the Secret-watch resumes the
// reconcile when the Secret is created.
func (r *IdentityServerClusterReconciler) handleAdminCredsSecretMissing(
	ctx context.Context,
	cluster *v1alpha1.IdentityServerCluster,
	writeErr error,
) (ctrl.Result, bool, error) {
	if !errors.Is(writeErr, errAdminCredsSecretMissing) {
		return ctrl.Result{}, false, nil
	}

	name := cluster.Spec.AdminCredentials.ValueFrom.SecretKeyRef.Name

	degChanged := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonAdminCredsSecretMissing,
		Message:            fmt.Sprintf("adminCredentials Secret %q does not exist; waiting for it to be created", name),
		ObservedGeneration: cluster.Generation,
	})
	readyChanged := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonAdminCredsSecretMissing,
		Message:            "admin credentials Secret is missing; see Degraded condition",
		ObservedGeneration: cluster.Generation,
	})
	if !degChanged && !readyChanged {
		return ctrl.Result{}, true, nil
	}

	cluster.Status.ObservedGeneration = cluster.Generation

	if err := r.Status().Update(ctx, cluster); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, true, nil
		}
		return ctrl.Result{}, true, fmt.Errorf("updating status for missing admin-creds Secret: %w", err)
	}

	r.Recorder.Eventf(cluster, corev1.EventTypeWarning, v1alpha1.ReasonAdminCredsSecretMissing,
		"adminCredentials Secret %q does not exist; waiting for it to be created", name)

	return ctrl.Result{}, true, nil
}
