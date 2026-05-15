package daemon

import (
	"context"
	cryptorand "crypto/rand"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const initialReconcileSource = "initial-reconcile"

type repaveReconciler struct {
	runner *daemonRunner
	// initialEvents queues the first reconcile after manager startup so cached Node
	// reads can detect state that existed before the daemon started and then keep
	// polling for ARM-only machine transitions.
	initialEvents            chan event.TypedGenericEvent[string]
	machineReconcileInterval time.Duration
}

func newRepaveReconciler(runner *daemonRunner) *repaveReconciler {
	return &repaveReconciler{
		runner:                   runner,
		initialEvents:            make(chan event.TypedGenericEvent[string], 1),
		machineReconcileInterval: runner.machineReconcileInterval,
	}
}

func (r *repaveReconciler) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b.Watches(
		&corev1.Node{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapNode),
		builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return e.Object.GetName() == r.runner.nodeName },
			UpdateFunc:  func(e event.UpdateEvent) bool { return e.ObjectNew.GetName() == r.runner.nodeName },
			DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetName() == r.runner.nodeName },
			GenericFunc: func(e event.GenericEvent) bool { return e.Object.GetName() == r.runner.nodeName },
		}),
	).WatchesRawSource(source.TypedChannel(
		r.initialEvents,
		handler.TypedEnqueueRequestsFromMapFunc(r.mapInitialEvent),
	))
}

func (r *repaveReconciler) ReconcileRepave(ctx context.Context, source string) (reconcile.Result, error) {
	if err := r.runner.reconcileOnce(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if source == initialReconcileSource {
		return reconcile.Result{RequeueAfter: r.machineReconcileInterval + machineReconcileJitter(r.machineReconcileInterval)}, nil
	}
	return reconcile.Result{}, nil
}

func machineReconcileJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	maxJitter := interval / 10
	if maxJitter <= 0 {
		return 0
	}
	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxJitter)+1))
	if err != nil {
		return 0
	}
	return time.Duration(jitter.Int64())
}

func (r *repaveReconciler) mapInitialEvent(_ context.Context, name string) []agentdaemon.Request {
	return []agentdaemon.Request{agentdaemon.NewRepaveRequest(name)}
}

func (r *repaveReconciler) mapNode(_ context.Context, obj client.Object) []agentdaemon.Request {
	if obj.GetName() != r.runner.nodeName {
		return nil
	}
	return []agentdaemon.Request{agentdaemon.NewRepaveRequest("node-change")}
}

func (r *repaveReconciler) enqueueInitialReconcile(ctx context.Context) {
	select {
	case r.initialEvents <- event.TypedGenericEvent[string]{Object: initialReconcileSource}:
	case <-ctx.Done():
	}
}

var _ agentdaemon.RepaveReconciler = (*repaveReconciler)(nil)
