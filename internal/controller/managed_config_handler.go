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

// newClusterManagedConfigHandler wraps newManagedConfigHandler so the
// implicit-default log fires once per Create/Update event — never on Delete
// or on the pre-Update snapshot.
func newClusterManagedConfigHandler(list managedConfigMapFunc) handler.EventHandler {
	inner := newManagedConfigHandler(list)
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

// managedConfigMapFunc lists reconcile requests for the clusters/nodes that
// an object's managed-config label + curity.io/cluster annotation designate.
type managedConfigMapFunc func(ctx context.Context, obj client.Object) []ctrl.Request

// newManagedConfigHandler returns an EventHandler that unions the old and new
// scope sets on Update events so clusters both leaving and entering scope
// reconcile in one cycle.
func newManagedConfigHandler(list managedConfigMapFunc) handler.EventHandler {
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
