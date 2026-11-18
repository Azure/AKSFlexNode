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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const machineOperationModeDisable = "disable"
const machineOperationModeAuto = "auto"

type machineOperationReconcilerOptions struct {
	Client               client.Client
	Reader               client.Reader // the API server, not the manager's cache
	Log                  *slog.Logger
	NodeName             string
	AKSMachineName       string
	MachineOperationMode string
	Runtime              agentruntime.Runtime
	State                stateStore
	Config               *config.Config
	Uninstall            func(*slog.Logger) phases.Task
	ClearConfig          phases.Task
	Resolve              machineGoalResolver
	AgentUpgrade         agentUpgradeExecutor
	Gate                 *startupGate
}

type machineOperationHandlers struct {
	log          *slog.Logger
	runtime      agentruntime.Runtime
	state        stateStore
	cfg          *config.Config
	resolve      machineGoalResolver
	uninstall    func(*slog.Logger) phases.Task
	clearConfig  phases.Task
	agentUpgrade agentUpgradeExecutor
	gate         *startupGate
	client       client.Client
	reader       client.Reader
	machineName  string
	nodeName     string
	readBootID   func() (string, error)
	rebootHost   func(context.Context, *slog.Logger) error
}

// machineOperationReconciler runs MachineOperations when the Machina CRD is available.
// TODO: Add a new machineOperationMode value when ARM-backed MachineOperations are supported.
func machineOperationReconciler(
	opts machineOperationReconcilerOptions,
) (daemon.MachineOperationRequestReconciler, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("kubernetes client is nil")
	}
	if opts.Runtime == nil {
		return nil, fmt.Errorf("worker runtime is nil")
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
	if opts.State == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if opts.Uninstall == nil {
		return nil, fmt.Errorf("uninstall task factory is nil")
	}
	if opts.ClearConfig == nil {
		return nil, fmt.Errorf("config cleanup task is nil")
	}
	if opts.Resolve == nil {
		return nil, fmt.Errorf("machine goal resolver is nil")
	}
	if opts.Gate == nil {
		return nil, fmt.Errorf("startup gate is nil")
	}

	handlers := &machineOperationHandlers{
		log:          opts.Log,
		runtime:      opts.Runtime,
		state:        opts.State,
		cfg:          opts.Config,
		resolve:      opts.Resolve,
		uninstall:    opts.Uninstall,
		clearConfig:  opts.ClearConfig,
		agentUpgrade: opts.AgentUpgrade,
		gate:         opts.Gate,
		client:       opts.Client,
		reader:       opts.Reader,
		machineName:  opts.AKSMachineName,
		nodeName:     opts.NodeName,
		readBootID:   readBootID,
		rebootHost:   daemon.RebootHost,
	}
	// A previously queued event or retry can run after an operation finishes.
	// Read its current status from the API server to avoid replaying the action.
	operationHandlers := daemon.MachineOperationHandlers{
		machinav1alpha3.OperationNodeReboot:     handlers.reconcileNodeReboot,
		machinav1alpha3.OperationAgentUpgrade:   handlers.reconcileAgentUpgrade,
		machinav1alpha3.OperationAgentReset:     handlers.reconcileAgentReset,
		machinav1alpha3.OperationHostSoftReboot: handlers.reconcileHostSoftReboot,
	}
	reconciler, err := daemon.NewMachinaMachineOperationReconcilerWithReader(
		opts.Client,
		opts.Reader,
		opts.AKSMachineName,
		opts.NodeName,
		operationHandlers,
	)
	if err != nil {
		return nil, fmt.Errorf("create MachineOperation reconciler: %w", err)
	}
	opts.Log.Info(
		"Machina MachineOperation API found; enabling machine operation reconciler",
	)
	return &flexMachineOperationReconciler{
		delegate:    reconciler,
		handlers:    operationHandlers,
		reader:      opts.Reader,
		machineName: opts.AKSMachineName,
		nodeName:    opts.NodeName,
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
	if err := store.MarkInProgress(ctx, op, "restarting active worker node"); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark NodeReboot MachineOperation in progress: %w", err)
	}
	if err := h.restartNode(ctx); err != nil {
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
		Message: "NodeReboot completed",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish NodeReboot MachineOperation: %w", err)
	}
	return ctrl.Result{}, nil
}

func (h *machineOperationHandlers) restartNode(ctx context.Context) error {
	active, err := activeMachineFromStore(ctx, h.state)
	if err != nil {
		return err
	}
	cfg := h.cfg.DeepCopy()
	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:   active.Name,
			Config: active.State.NodeConfig,
		},
	}
	if active.State.Isolation == isolation.ModeNSpawn {
		if active.State.AppliedKubernetesVersion != "" {
			cfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
		}
		resolved, err := h.resolve(ctx, h.log, cfg, active.Name)
		if err != nil {
			return fmt.Errorf("resolve goal state for node restart: %w", err)
		}
		input.Node.GoalState = resolved.GoalState
		input.Archives = resolved.ContainerImageArchives
	}
	return h.runtime.Restart(h.log, input).Do(ctx)
}

func (h *machineOperationHandlers) reconcileAgentUpgrade(
	ctx context.Context,
	store daemon.MachineOperationStore[int64],
	op daemon.MachineOperation,
) (ctrl.Result, error) {
	request, err := parseAgentUpgradeRequest(op.Parameters)
	if err != nil {
		return h.finishFailedMachineOperation(ctx, store, op, "InvalidParameters", err.Error())
	}
	if err := h.agentUpgrade.Validate(request); err != nil {
		return h.finishFailedMachineOperation(ctx, store, op, "InvalidParameters", err.Error())
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
	if err := h.agentUpgrade.RecordPending(ctx, op.Name); err != nil {
		if errors.Is(err, errAgentUpgradeAlreadyPending) {
			// Retry a previously failed recovery handoff. An ordinary duplicate
			// remains a no-op while the delayed daemon restart is pending.
			restarted, err := h.agentUpgrade.RetryRecovery(ctx)
			if restarted {
				h.gate.stopping.Store(true)
			}
			return ctrl.Result{}, err
		}
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", err.Error())
	}
	if err := store.MarkInProgress(ctx, op, "staging upgraded AKS Flex Node agent"); err != nil {
		cleanupCtx, cancel := agentdeployment.UpgradeCleanupContext(ctx)
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
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", err.Error())
	}
	if err := h.agentUpgrade.Restart(ctx); err != nil {
		if abortErr := h.agentUpgrade.Abort(ctx); abortErr != nil {
			return h.beginAgentUpgradeRecovery(ctx, op, err, abortErr)
		}
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", "failed to restart upgraded agent daemon")
	}
	h.gate.stopping.Store(true)
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
	cleanupCtx, cancel := agentdeployment.UpgradeCleanupContext(ctx)
	defer cancel()
	restartErr := h.agentUpgrade.Restart(cleanupCtx)
	if restartErr != nil {
		h.log.Error("failed to restart daemon for AgentUpgrade recovery", "operation", op.Name, "error", restartErr)
	}
	if restartErr == nil {
		h.gate.stopping.Store(true)
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
	if err := store.MarkInProgress(
		ctx,
		op,
		"resetting local worker runtime",
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark AgentReset MachineOperation in progress: %w", err)
	}
	if err := phases.ExecuteTask(ctx, h.log, ResetWorker(h.log, h.runtime, yurthub.ResetHostTask(h.log))); err != nil {
		return h.finishFailedMachineOperation(
			ctx,
			store,
			op,
			"ExecutionFailed",
			err.Error(),
		)
	}
	if err := phases.ExecuteTask(ctx, h.log, h.clearConfig); err != nil {
		return h.finishFailedMachineOperation(ctx, store, op, "ExecutionFailed", fmt.Sprintf("%s: %v", h.clearConfig.Name(), err))
	}
	// FlexNode does not have a Machina Machine CR, so observed machine
	// generation is intentionally unset.
	if err := store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: "AgentReset completed",
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish AgentReset MachineOperation: %w", err)
	}
	if err := phases.ExecuteTask(ctx, h.log, h.uninstall(h.log)); err != nil {
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
	if err := store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseFailed,
		Reason:  reason,
		Message: message,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("finish failed MachineOperation: %w", err)
	}
	return ctrl.Result{}, nil
}
