package daemon

import (
	"context"
	"errors"
	"fmt"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errMachineOperationNoLongerCurrent = errors.New("MachineOperation was removed, replaced, or completed")

type machineOperationIdentityKey struct{}

type machineOperationIdentity struct {
	name string
	uid  types.UID
}

type machineOperationIdentityReconciler struct {
	daemon.MachineOperationRequestReconciler
	reader   client.Reader
	handlers *machineOperationHandlers
}

func (r *machineOperationIdentityReconciler) ReconcileMachineOperation(ctx context.Context, name string) (ctrl.Result, error) {
	var op machinav1alpha3.MachineOperation
	err := r.reader.Get(ctx, client.ObjectKey{Name: name}, &op)
	if apierrors.IsNotFound(err) {
		delete(r.handlers.pendingResults, name)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read MachineOperation identity: %w", err)
	}
	// The shared reconciler skips terminal/missing objects before calling a
	// handler, so cached results must be retired at the request boundary.
	if op.Status.IsTerminal() {
		delete(r.handlers.pendingResults, name)
		return ctrl.Result{}, nil
	}
	if pending, ok := r.handlers.pendingResults[name]; ok && pending.uid != op.UID {
		delete(r.handlers.pendingResults, name)
	}
	ctx = context.WithValue(ctx, machineOperationIdentityKey{}, machineOperationIdentity{name: name, uid: op.UID})
	result, err := r.MachineOperationRequestReconciler.ReconcileMachineOperation(ctx, name)
	if errors.Is(err, errMachineOperationNoLongerCurrent) {
		delete(r.handlers.pendingResults, name)
	}
	return result, err
}

type machineOperationIdentityClient struct {
	client.Client
	reader client.Reader
}

func (c *machineOperationIdentityClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	op, ok := obj.(*machinav1alpha3.MachineOperation)
	if !ok {
		return c.Client.Get(ctx, key, obj, opts...)
	}
	err := c.reader.Get(ctx, key, obj, opts...)
	identity, guarded := ctx.Value(machineOperationIdentityKey{}).(machineOperationIdentity)
	if guarded && key.Name == identity.name {
		if apierrors.IsNotFound(err) || err == nil && (op.UID != identity.uid || op.Status.IsTerminal()) {
			return errMachineOperationNoLongerCurrent
		}
	}
	// Shared MarkInProgress/Finish retry conflicts by reading the latest object
	// by name. Check its UID on every uncached read. Their subsequent status
	// Update carries this UID and resourceVersion, so a replacement racing the
	// write is rejected by the API server rather than receiving the old result.
	return err
}
