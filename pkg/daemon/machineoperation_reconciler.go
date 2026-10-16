package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

const machineOperationModeDisable = "disable"
const machineOperationModeAuto = "auto"

type machineOperationReconcilerOptions struct {
	Client               client.Client
	Reader               client.Reader
	Log                  *slog.Logger
	NodeName             string
	AKSMachineName       string
	MachineOperationMode string
	Operator             nodeOperator
	AgentUpgrade         agentUpgradeExecutor
}

type machineOperationHandlers struct {
	log          *slog.Logger
	operator     nodeOperator
	agentUpgrade agentUpgradeExecutor
	reader       client.Reader
	machineName  string
	// The shared controller serializes handlers. Retain terminal results across
	// status-write retries rather than executing host side effects again.
	pendingResults map[string]pendingMachineOperationResult
}

type pendingMachineOperationResult struct {
	uid    types.UID
	result daemon.MachineOperationResult[int64]
}

// machineOperationReconciler runs MachineOperations when the Machina CRD is available.
// TODO: Add a new machineOperationMode value when ARM-backed MachineOperations are supported.
func machineOperationReconciler(
	opts machineOperationReconcilerOptions,
) (daemon.MachineOperationRequestReconciler, error) {
	if opts.Log == nil {
		return nil, fmt.Errorf("logger is nil")
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("kubernetes client is nil")
	}
	if opts.Operator == nil {
		return nil, fmt.Errorf("node operator is nil")
	}
	if opts.AgentUpgrade == nil {
		return nil, fmt.Errorf("agent upgrade executor is nil")
	}
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
	if opts.NodeName == "" {
		return nil, fmt.Errorf("node name is empty")
	}
	if opts.AKSMachineName == "" {
		return nil, fmt.Errorf("AKS machine name is empty")
	}

	handlers := &machineOperationHandlers{
		log:          opts.Log,
		operator:     opts.Operator,
		agentUpgrade: opts.AgentUpgrade,
		reader:       opts.Reader,
		machineName:  opts.AKSMachineName,
	}
	if handlers.reader == nil {
		handlers.reader = opts.Client
	}
	operationClient := &machineOperationIdentityClient{Client: opts.Client, reader: handlers.reader}
	reconciler, err := daemon.NewMachinaMachineOperationReconcilerWithReader(
		operationClient,
		operationClient,
		opts.AKSMachineName,
		opts.NodeName,
		daemon.MachineOperationHandlers{
			machinav1alpha3.OperationNodeReboot:   handlers.reconcileNodeReboot,
			machinav1alpha3.OperationAgentUpgrade: handlers.reconcileAgentUpgrade,
			machinav1alpha3.OperationAgentReset:   handlers.reconcileAgentReset,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create MachineOperation reconciler: %w", err)
	}
	opts.Log.Info(
		"Machina MachineOperation API found; enabling machine operation reconciler",
	)
	return &machineOperationIdentityReconciler{
		MachineOperationRequestReconciler: reconciler,
		reader:                            handlers.reader,
		handlers:                          handlers,
	}, nil
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
	if result, ok := h.pendingResult(ctx, op.Name); ok {
		return h.finishMachineOperation(ctx, store, op, result)
	}
	generation, err := observedMachineGeneration(ctx, h.reader, h.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := store.MarkInProgress(ctx, op, "restarting active nspawn node"); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark NodeReboot MachineOperation in progress: %w", err)
	}
	if err := h.operator.RestartNode(ctx, h.log); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
			generation,
		)
	}
	return h.finishMachineOperation(ctx, store, op, daemon.MachineOperationResult[int64]{
		Phase:                     machinav1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   "NodeReboot completed",
		ObservedMachineGeneration: generation,
	})
}

func (h *machineOperationHandlers) reconcileAgentUpgrade(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	if result, ok := h.pendingResult(ctx, op.Name); ok {
		return h.finishMachineOperation(ctx, store, op, result)
	}
	request, err := parseAgentUpgradeRequest(op.Parameters)
	if err != nil {
		return h.finishFailedMachineOperation(ctx, store, op, "InvalidParameters", err.Error(), 0)
	}
	activationLock, err := h.agentUpgrade.Acquire()
	if errors.Is(err, agentbinary.ErrActivationInProgress) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("acquire agent activation lock: %w", err)
	}
	defer func() {
		if closeErr := activationLock.Close(); closeErr != nil {
			h.log.Warn("failed to release agent activation lock", "error", closeErr)
		}
	}()
	// Persist the recovery signal before InProgress. The shared reconciler does
	// not enqueue InProgress operations after a process crash, so the signal
	// must exist before the status can become non-reconcilable.
	generation, err := h.agentUpgrade.RecordPending(ctx, op.Name)
	if err != nil {
		if errors.Is(err, errAgentUpgradeAlreadyPending) {
			// Retry a previously failed recovery handoff. An ordinary duplicate
			// remains a no-op while the delayed daemon restart is pending.
			return ctrl.Result{}, h.agentUpgrade.RetryRecovery(ctx)
		}
		return ctrl.Result{}, fmt.Errorf("record pending AgentUpgrade: %w", err)
	}
	if err := store.MarkInProgress(ctx, op, "staging upgraded AKS Flex Node agent binary"); err != nil {
		cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
		abortErr := h.agentUpgrade.Abort(cleanupCtx)
		cancel()
		return ctrl.Result{}, errors.Join(
			fmt.Errorf("mark AgentUpgrade MachineOperation in progress: %w", err),
			wrapOptionalError("clear pending AgentUpgrade signal", abortErr),
		)
	}
	if err := h.agentUpgrade.Stage(ctx, request); err != nil {
		if abortErr := h.agentUpgrade.Abort(ctx); abortErr != nil {
			return h.beginAgentUpgradeRecovery(ctx, op, err, abortErr)
		}
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", err.Error(), generation)
	}
	if err := h.agentUpgrade.Restart(ctx); err != nil {
		if abortErr := h.agentUpgrade.Abort(ctx); abortErr != nil {
			return h.beginAgentUpgradeRecovery(ctx, op, err, abortErr)
		}
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", "failed to restart upgraded agent daemon", generation)
	}
	// Restart scheduling is the old daemon's final responsibility. Keep the
	// operation InProgress and let the restarted or recovery daemon publish the
	// only terminal result.
	return ctrl.Result{}, nil
}

func (h *machineOperationHandlers) beginAgentUpgradeRecovery(
	ctx context.Context,
	op daemon.MachineOperation,
	executionErr, rollbackErr error,
) (ctrl.Result, error) {
	message := fmt.Sprintf("AgentUpgrade execution failed and requires recovery: %v", executionErr)
	recordErr := h.agentUpgrade.RecordFailure(message)
	if recordErr != nil {
		h.log.Error("failed to annotate durable AgentUpgrade recovery signal", "operation", op.Name, "error", recordErr)
	}
	h.log.Error("AgentUpgrade rollback failed; restarting daemon for durable recovery",
		"operation", op.Name,
		"error", rollbackErr,
	)
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	restartErr := h.agentUpgrade.Restart(cleanupCtx)
	if restartErr != nil {
		h.log.Error("failed to restart daemon for AgentUpgrade recovery", "operation", op.Name, "error", restartErr)
	}
	if recordErr != nil || restartErr != nil {
		return ctrl.Result{}, errors.Join(
			wrapOptionalError("record AgentUpgrade recovery failure", recordErr),
			wrapOptionalError("restart daemon for AgentUpgrade recovery", restartErr),
		)
	}
	return ctrl.Result{}, nil
}

func (h *machineOperationHandlers) reconcileAgentReset(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	if result, ok := h.pendingResult(ctx, op.Name); ok {
		if _, err := h.finishMachineOperation(ctx, store, op, result); err != nil {
			return ctrl.Result{}, err
		}
		if result.Phase == machinav1alpha3.OperationPhaseComplete {
			return ctrl.Result{}, h.operator.StopDaemon(ctx, h.log)
		}
		return ctrl.Result{}, nil
	}
	generation, err := observedMachineGeneration(ctx, h.reader, h.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := store.MarkInProgress(
		ctx,
		op,
		"resetting local nspawn node runtime",
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark AgentReset MachineOperation in progress: %w", err)
	}
	if err := h.operator.ResetNode(ctx, h.log); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
			generation,
		)
	}
	if _, err := h.finishMachineOperation(ctx, store, op, daemon.MachineOperationResult[int64]{
		Phase:                     machinav1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   "AgentReset completed",
		ObservedMachineGeneration: generation,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish AgentReset MachineOperation: %w", err)
	}
	if err := h.operator.StopDaemon(ctx, h.log); err != nil {
		return ctrl.Result{}, fmt.Errorf("stop daemon after AgentReset MachineOperation: %w", err)
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
		0,
	)
}

func (h *machineOperationHandlers) finishFailedMachineOperation(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
	reason, message string,
	generation int64,
) (ctrl.Result, error) {
	return h.finishMachineOperation(ctx, store, op, daemon.MachineOperationResult[int64]{
		Phase:                     machinav1alpha3.OperationPhaseFailed,
		Reason:                    reason,
		Message:                   message,
		ObservedMachineGeneration: generation,
	})
}

func (h *machineOperationHandlers) finishMachineOperation(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation, result daemon.MachineOperationResult[int64]) (ctrl.Result, error) {
	if h.pendingResults == nil {
		h.pendingResults = make(map[string]pendingMachineOperationResult)
	}
	identity, _ := ctx.Value(machineOperationIdentityKey{}).(machineOperationIdentity)
	h.pendingResults[op.Name] = pendingMachineOperationResult{uid: identity.uid, result: result}
	if err := store.Finish(ctx, op, result); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish MachineOperation: %w", err)
	}
	delete(h.pendingResults, op.Name)
	return ctrl.Result{}, nil
}

func (h *machineOperationHandlers) pendingResult(ctx context.Context, name string) (daemon.MachineOperationResult[int64], bool) {
	pending, ok := h.pendingResults[name]
	identity, _ := ctx.Value(machineOperationIdentityKey{}).(machineOperationIdentity)
	if ok && pending.uid != identity.uid {
		delete(h.pendingResults, name)
		return daemon.MachineOperationResult[int64]{}, false
	}
	return pending.result, ok
}

// Use an uncached reader: publication is optional and must not create a Machine
// informer or make cache startup depend on the Machine API.
func observedMachineGeneration(ctx context.Context, reader client.Reader, name string) (int64, error) {
	if reader == nil {
		return 0, nil
	}
	var machine machinav1alpha3.Machine
	err := reader.Get(ctx, client.ObjectKey{Name: name}, &machine)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read projected Machine %s generation: %w", name, err)
	}
	return machine.Generation, nil
}
