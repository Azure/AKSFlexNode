package daemon

import (
	"context"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"

	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type startupGate struct {
	ready chan struct{}
	once  sync.Once
}

func newStartupGate() *startupGate {
	return &startupGate{ready: make(chan struct{})}
}

func (g *startupGate) open() {
	g.once.Do(func() { close(g.ready) })
}

func (g *startupGate) wait(ctx context.Context) error {
	select {
	case <-g.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type gatedMachineOperationReconciler struct {
	delegate agentdaemon.MachineOperationRequestReconciler
	gate     *startupGate
}

func (r gatedMachineOperationReconciler) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return r.delegate.SetupController(b)
}

func (r gatedMachineOperationReconciler) ReconcileMachineOperation(ctx context.Context, name string) (ctrl.Result, error) {
	if err := r.gate.wait(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.delegate.ReconcileMachineOperation(ctx, name)
}

type gatedRepaveReconciler struct {
	delegate agentdaemon.RepaveReconciler
	gate     *startupGate
}

func (r gatedRepaveReconciler) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return r.delegate.SetupController(b)
}

func (r gatedRepaveReconciler) ReconcileRepave(ctx context.Context, source string) (ctrl.Result, error) {
	if err := r.gate.wait(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.delegate.ReconcileRepave(ctx, source)
}
