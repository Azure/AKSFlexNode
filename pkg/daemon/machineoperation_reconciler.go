package daemon

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

const machineOperationModeDisable = "disable"
const machineOperationModeAuto = "auto"

type machineOperationReconcilerOptions struct {
	Client               client.Client
	Log                  *slog.Logger
	NodeName             string
	AKSMachineName       string
	MachineOperationMode string
	Operator             NodeOperator
}

type machineOperationHandlers struct {
	log      *slog.Logger
	operator NodeOperator
}

// machineOperationReconciler runs MachineOperations when the Machina CRD is available.
// TODO: Add a new machineOperationMode value when ARM-backed MachineOperations are supported.
func machineOperationReconciler(
	opts machineOperationReconcilerOptions,
) (daemon.MachineOperationRequestReconciler, error) {
	if opts.MachineOperationMode == "" {
		opts.MachineOperationMode = machineOperationModeAuto
	}
	if opts.MachineOperationMode == machineOperationModeDisable {
		opts.Log.Debug(
			"Machina MachineOperation support disabled; using noop machine operation reconciler",
		)
		return daemon.NoopMachineOperationReconciler(), nil
	}

	present, err := hasMachineOperationAPI(opts.Client)
	if err != nil {
		return nil, err
	}
	if !present {
		opts.Log.Debug(
			"Machina MachineOperation API not found; using noop machine operation reconciler",
		)
		return daemon.NoopMachineOperationReconciler(), nil
	}

	handlers := &machineOperationHandlers{log: opts.Log, operator: opts.Operator}
	reconciler, err := daemon.NewMachinaMachineOperationReconciler(
		opts.Client,
		opts.NodeName,
		opts.AKSMachineName,
		daemon.MachineOperationHandlers{
			machinav1alpha3.OperationNodeReboot:   handlers.reconcileNodeReboot,
			machinav1alpha3.OperationAgentUpgrade: handlers.unsupportedOperation,
			machinav1alpha3.OperationAgentReset:   handlers.reconcileAgentReset,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create MachineOperation reconciler: %w", err)
	}
	opts.Log.Info(
		"Machina MachineOperation API found; enabling machine operation reconciler",
	)
	return reconciler, nil
}

func hasMachineOperationAPI(c client.Client) (bool, error) {
	_, err := c.RESTMapper().RESTMapping(schema.GroupKind{
		Group: machinav1alpha3.GroupVersion.Group,
		Kind:  "MachineOperation",
	}, machinav1alpha3.GroupVersion.Version)
	if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("discover Machina MachineOperation API: %w", err)
	}
	return true, nil
}

func (h *machineOperationHandlers) reconcileNodeReboot(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	if err := store.MarkInProgress(ctx, op, "restarting active nspawn node"); err != nil {
		return ctrl.Result{}, err
	}
	state, err := h.operator.LoadState(ctx)
	if err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	active, err := h.operator.FindActiveMachine(ctx, h.log, state)
	if err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	if err := h.operator.RestartNode(ctx, h.log, active); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	// FlexNode does not have a Machina Machine CR, so observed machine
	// generation is intentionally unset.
	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: "NodeReboot completed",
	})
}

func (h *machineOperationHandlers) reconcileAgentReset(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	if err := store.MarkInProgress(
		ctx,
		op,
		"resetting local nspawn node runtime",
	); err != nil {
		return ctrl.Result{}, err
	}
	if err := h.operator.ResetNodeRuntime(ctx, h.log); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	if err := h.operator.ClearState(ctx); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	// FlexNode does not have a Machina Machine CR, so observed machine
	// generation is intentionally unset.
	if err := store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: "AgentReset completed",
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := h.operator.StopDaemon(ctx, h.log); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (h *machineOperationHandlers) unsupportedOperation(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	return h.finishFailedMachineOperation(
		ctx,
		store,
		op,
		"UnsupportedOperation",
		fmt.Sprintf(
			"operation kind %s is not supported by AKS FlexNode daemon",
			op.Kind,
		),
	)
}

func (h *machineOperationHandlers) finishFailedMachineOperation(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
	reason, message string,
) (ctrl.Result, error) {
	// FlexNode does not have a Machina Machine CR, so observed machine
	// generation is intentionally unset.
	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseFailed,
		Reason:  reason,
		Message: message,
	})
}
