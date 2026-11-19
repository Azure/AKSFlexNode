package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

var errAgentUpgradeAlreadyPending = errors.New("AgentUpgrade operation is already pending")

// parseAgentUpgradeRequest reads an AgentUpgrade's parameters and checks what every agent requires
// of them: a download URL and a SHA-256. Which URLs an agent takes, an archive's or an image's, is
// its executor's to check; see agentUpgradeExecutor.Validate.
func parseAgentUpgradeRequest(parameters map[string]string) (agentdeployment.UpgradeRequest, error) {
	request := agentdeployment.UpgradeRequest{
		DownloadURL: strings.TrimSpace(parameters[agentdeployment.UpgradeDownloadURLParameter]),
		SHA256:      strings.TrimSpace(parameters[agentdeployment.UpgradeSHA256Parameter]),
	}
	if request.DownloadURL == "" {
		return agentdeployment.UpgradeRequest{}, fmt.Errorf("missing required parameter %q", agentdeployment.UpgradeDownloadURLParameter)
	}
	if _, err := agentdeployment.UpgradeDigest(request.SHA256); err != nil {
		return agentdeployment.UpgradeRequest{}, err
	}
	return request, nil
}

// The coordinator combines deployment mechanics with daemon-owned State and publication.
type agentUpgradeCoordinator struct {
	agentdeployment.UpgradeExecutor
	state      stateStore
	instanceID string
}

func (u *agentUpgradeCoordinator) RecordPending(ctx context.Context, operationName string) error {
	return recordPendingAgentUpgrade(ctx, u.SignalStore(), u.state, operationName, u.instanceID)
}

func (u *agentUpgradeCoordinator) DaemonInstance() string { return u.instanceID }

type agentUpgradeExecutor interface {
	Acquire() (io.Closer, error)
	Validate(agentdeployment.UpgradeRequest) error
	RecordPending(context.Context, string) error
	RetryRecovery(context.Context) (bool, error)
	RecordFailure(string) error
	Stage(context.Context, agentdeployment.UpgradeRequest) error
	Abort(context.Context) error
	Restart(context.Context) error
}

type agentUpgradeHandoff interface {
	SignalStore() agentdeployment.UpgradeSignalStore
	DaemonInstance() string
	ValidateStarted(*agentdeployment.UpgradeSignal) error
	CandidateActive(*agentdeployment.UpgradeSignal) (bool, error)
	RollbackSwitch(*agentdeployment.UpgradeSignal) error
	RunningLastGood(*agentdeployment.UpgradeSignal) (bool, error)
	Restart(context.Context) error
}

func recordPendingAgentUpgrade(
	ctx context.Context,
	signals agentdeployment.UpgradeSignalStore,
	state stateStore,
	operationName, instanceID string,
) error {
	existing, err := signals.Read()
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.OperationName == operationName {
			return errAgentUpgradeAlreadyPending
		}
		return fmt.Errorf("another AgentUpgrade operation %q is pending", existing.OperationName)
	}
	loaded, err := state.Load(ctx)
	if err != nil {
		return fmt.Errorf("load daemon state for AgentUpgrade: %w", err)
	}
	if loaded == nil || !validActiveMachine(loaded.ActiveMachine) {
		return fmt.Errorf("no valid active worker for AgentUpgrade")
	}
	if err := signals.RecordPending(operationName, loaded.ActiveMachine, instanceID); err != nil {
		return err
	}
	return nil
}

// agentUpgradeHelperPaths resolves both worker slots because a later repave can
// change the active slot without replacing the daemon's upgrade executor.
func agentUpgradeHelperPaths(log *slog.Logger, mode isolation.Mode) (map[string]string, error) {
	if mode == isolation.ModeNone {
		return nil, nil
	}

	runtime, err := agentruntime.New(mode)
	if err != nil {
		return nil, err
	}
	helpers := make(map[string]string, 2)
	for _, slot := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		if mode == isolation.ModeContainer {
			// The private rootfs path needs no new goal resolution or device discovery.
			helpers[slot] = filepath.Join((container.Layout{}).RootfsDir(slot), "usr/local/bin/aks-flex-node")
			continue
		}

		worker, err := runtime.Worker(log, agentruntime.Node{Slot: slot})
		if err != nil {
			return nil, fmt.Errorf("resolve worker for agent upgrade: %w", err)
		}
		helpers[slot] = worker.Root.Path("/usr/local/bin/aks-flex-node")
	}

	return helpers, nil
}

// RecoverAgentUpgrade selects the applied worker before deployment restores the
// last-good agent. Image deployments have no host worker mode to select.
func RecoverAgentUpgrade(ctx context.Context, log *slog.Logger, message string) error {
	if agentdeployment.Containerized() {
		return agentdeployment.RecoverAgentUpgrade(ctx, log, message, config.DefaultIsolationMode, nil)
	}

	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load applied worker for AgentUpgrade recovery: %w", err)
	}
	mode := config.DefaultIsolationMode
	if state != nil {
		mode = state.Isolation
	}
	helpers, err := agentUpgradeHelperPaths(log, mode)
	if err != nil {
		return err
	}

	return agentdeployment.RecoverAgentUpgrade(ctx, log, message, mode, helpers)
}

func publishAndClearAgentUpgradeSignal(ctx context.Context, log *slog.Logger, c client.Client, upgrade agentUpgradeHandoff) error {
	signals := upgrade.SignalStore()
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return nil
	}
	if signal.Failure == "" && signal.InitiatingDaemonInstance == upgrade.DaemonInstance() {
		// The initiating daemon must not consume its own signal while staging.
		return nil
	}

	if signal.Failure == "" {
		if validationErr := upgrade.ValidateStarted(signal); validationErr != nil {
			signal.Failure = validationErr.Error()
			candidateActive, activeErr := upgrade.CandidateActive(signal)
			if activeErr != nil {
				signal.Failure = errors.Join(validationErr, activeErr).Error()
				signal.RecoveryRequired = true
			} else {
				signal.RecoveryRequired = candidateActive
			}
			if err := signals.Write(*signal); err != nil {
				return err
			}
		}
	}

	result := agentdaemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: "AgentUpgrade completed",
	}
	if signal.Failure != "" {
		result.Phase = machinav1alpha3.OperationPhaseFailed
		result.Reason = "DaemonFailed"
		result.Message = signal.Failure

		if err := upgrade.RollbackSwitch(signal); err != nil {
			return fmt.Errorf("roll back failed AgentUpgrade: %w", err)
		}
	}

	if signal.RecoveryRequired {
		lastGood, err := upgrade.RunningLastGood(signal)
		if err != nil {
			return err
		}
		if !lastGood {
			cleanupCtx, cancel := agentdeployment.UpgradeCleanupContext(ctx)
			restartErr := upgrade.Restart(cleanupCtx)
			cancel()
			if restartErr != nil {
				return fmt.Errorf("restart last-good agent: %w", restartErr)
			}
			// Keep the signal and terminal status unpublished until the last-good
			// process confirms that it is running.
			return nil
		}
	}
	finishErr := agentdaemon.FinishMachineOperation(ctx, c, agentdaemon.MachineOperation{Name: signal.OperationName}, result)
	if finishErr != nil {
		return fmt.Errorf("publish AgentUpgrade result: %w", finishErr)
	}
	if err := signals.Clear(); err != nil {
		return err
	}
	log.Info("published AgentUpgrade result", "operation", signal.OperationName, "phase", result.Phase)
	return nil
}

func wrapOptionalError(context string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", context, err)
}

func newDaemonInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
