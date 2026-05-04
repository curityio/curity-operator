package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const implicitConfigTypeLogMsg = "treating curity.io/config-type as base (no value set)"

func logImplicitConfigTypeIfNeeded(ctx context.Context, obj client.Object) {
	if !hasManagedLabel(obj) {
		return
	}
	if obj.GetAnnotations()[AnnotationConfigType] != "" {
		return
	}
	kind := "ConfigMap"
	if _, ok := obj.(*corev1.Secret); ok {
		kind = "Secret"
	}
	ctrl.LoggerFrom(ctx).Info(implicitConfigTypeLogMsg,
		"kind", kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
}

// newClusterManagedConfigHandler wraps newUnionScopeHandler so the
// implicit-default log fires once per Create/Update event — never on Delete
// or on the pre-Update snapshot.
func newClusterManagedConfigHandler(list scopeMapFunc) handler.EventHandler {
	inner := newUnionScopeHandler(list)
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			logImplicitConfigTypeIfNeeded(ctx, e.Object)
			inner.Create(ctx, e, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			logImplicitConfigTypeIfNeeded(ctx, e.ObjectNew)
			inner.Update(ctx, e, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			inner.Delete(ctx, e, q)
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			inner.Generic(ctx, e, q)
		},
	}
}

// scopeMapFunc lists reconcile requests for the targets affected by an
// object's current state. Used by both the managed-config watches (where
// scope is derived from labels and curity.io/cluster annotations) and the
// node-clusterRef watch (where scope is the referenced cluster).
type scopeMapFunc func(ctx context.Context, obj client.Object) []ctrl.Request

// newUnionScopeHandler returns an EventHandler that unions the old and new
// map results on Update events so all reconcile targets affected by either
// the prior or new state fire in one cycle. Used both for managed-config
// label/annotation changes and for IdentityServerNode clusterRef edits.
func newUnionScopeHandler(list scopeMapFunc) handler.EventHandler {
	enqueue := func(q workqueue.TypedRateLimitingInterface[reconcile.Request], reqs []ctrl.Request) {
		for _, r := range reqs {
			q.Add(r)
		}
	}
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, list(ctx, e.Object))
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			seen := make(map[ctrl.Request]struct{})
			for _, r := range list(ctx, e.ObjectOld) {
				seen[r] = struct{}{}
			}
			for _, r := range list(ctx, e.ObjectNew) {
				seen[r] = struct{}{}
			}
			for r := range seen {
				q.Add(r)
			}
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, list(ctx, e.Object))
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			enqueue(q, list(ctx, e.Object))
		},
	}
}
