package daemon

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

// Flex has a Node but no Machina Machine CR. Queue selectors without consulting
// the cache; dispatch checks the current Node labels through the API reader.
type flexMachineOperationReconciler struct {
	delegate    agentdaemon.MachineOperationRequestReconciler
	handlers    agentdaemon.MachineOperationHandlers
	reader      client.Reader
	machineName string
	nodeName    string
}

func (r *flexMachineOperationReconciler) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b.Watches(&machinav1alpha3.MachineOperation{}, handler.TypedEnqueueRequestsFromMapFunc(r.mapOperation))
}

func (r *flexMachineOperationReconciler) mapOperation(_ context.Context, obj client.Object) []agentdaemon.Request {
	op, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok || !operationPending(op.Status.Phase) {
		return nil
	}
	if _, ok := r.handlers[op.Spec.OperationKind]; !ok || op.Spec.OperationKind.IsAllocationOperation() {
		return nil
	}
	if op.Spec.MachineRef != "" && op.Spec.MachineRef != r.machineName {
		return nil
	}
	if op.Spec.MachineRef == "" && op.Spec.MachineSelector == nil {
		return nil
	}

	return []agentdaemon.Request{agentdaemon.NewMachineOperationRequest(op.Name)}
}

func (r *flexMachineOperationReconciler) ReconcileMachineOperation(ctx context.Context, name string) (ctrl.Result, error) {
	var op machinav1alpha3.MachineOperation
	if err := r.reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if op.Status.IsTerminal() {
		return ctrl.Result{}, nil
	}

	// Startup, not another reconcile, judges a reboot already issued.
	if op.Spec.OperationKind == machinav1alpha3.OperationHostSoftReboot && !operationPending(op.Status.Phase) {
		return ctrl.Result{}, nil
	}

	matches, err := matchesFlexMachine(ctx, r.reader, &op, r.machineName, r.nodeName)
	if err != nil || !matches {
		return ctrl.Result{}, err
	}

	return r.delegate.ReconcileMachineOperation(ctx, name)
}
