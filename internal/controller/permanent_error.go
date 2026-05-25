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
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/curityio/curity-operator/api/v1alpha1"
)

// handlePermanentWriteError classifies an apiserver write error as permanent
// (apierrors.IsInvalid) versus transient. On permanent: sets Degraded+Ready,
// emits one Warning event, returns handled=true so the caller skips its
// existing wrap-and-bubble path. On anything else: handled=false and the
// caller falls through unchanged.
//
// Calls apimeta.SetStatusCondition directly (not the local setCondition
// wrapper) so the helper can capture whether conditions actually changed and
// skip the Status().Update + Event when they didn't. The For() watch lacks
// GenerationChangedPredicate, so a status write fires reconcile; without this
// gate the helper would self-trigger on every reconcile until apiserver
// no-op elision saved us (and only at the cost of inflating the Event count).
func (r *IdentityServerNodeReconciler) handlePermanentWriteError(
	ctx context.Context,
	node *v1alpha1.IdentityServerNode,
	resourceKind string,
	writeErr error,
) (ctrl.Result, bool, error) {
	if !apierrors.IsInvalid(writeErr) {
		return ctrl.Result{}, false, nil
	}

	msg := truncateReplicaFailureMessage(writeErr.Error())

	degChanged := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s rejected by apiserver: %s", resourceKind, msg),
		ObservedGeneration: node.Generation,
	})
	readyChanged := apimeta.SetStatusCondition(&node.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s is invalid; see Degraded condition", resourceKind),
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
		return ctrl.Result{}, true, fmt.Errorf("updating status after invalid %s: %w", resourceKind, err)
	}

	r.Recorder.Eventf(node, corev1.EventTypeWarning, v1alpha1.ReasonInvalidSpec,
		"%s rejected by apiserver: %s", resourceKind, msg)

	return ctrl.Result{}, true, nil
}

// handlePermanentWriteError classifies an apiserver IsInvalid write error
// as a permanent spec error: sets Degraded=True/InvalidSpec, emits one event,
// returns handled=true. Operational conditions are deliberately left alone
// (they observe pod state, not spec validity).
func (r *IdentityServerClusterReconciler) handlePermanentWriteError(
	ctx context.Context,
	cluster *v1alpha1.IdentityServerCluster,
	writeErr error,
) (ctrl.Result, bool, error) {
	if !apierrors.IsInvalid(writeErr) {
		return ctrl.Result{}, false, nil
	}

	// Extract apiserver text directly to skip the fmt.Errorf wrap that
	// sub-helpers (ensureAdminCredentialsSecret, ensureClusterConfig) apply
	// — otherwise the condition message ends up doubly-prefixed.
	var statusErr *apierrors.StatusError
	if !errors.As(writeErr, &statusErr) || statusErr == nil {
		return ctrl.Result{}, false, nil
	}

	kind := "Resource"
	if statusErr.ErrStatus.Details != nil && statusErr.ErrStatus.Details.Kind != "" {
		kind = statusErr.ErrStatus.Details.Kind
	}
	apiMsg := truncateReplicaFailureMessage(statusErr.ErrStatus.Message)

	degChanged := apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.ConditionDegraded,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonInvalidSpec,
		Message:            fmt.Sprintf("%s rejected by apiserver: %s", kind, apiMsg),
		ObservedGeneration: cluster.Generation,
	})

	if !degChanged {
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
