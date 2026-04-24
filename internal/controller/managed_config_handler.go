package controller

import (
	"context"

	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

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
