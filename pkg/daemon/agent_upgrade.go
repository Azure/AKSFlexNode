package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeSHA256Parameter      = "sha256"
)

var errAgentUpgradeAlreadyPending = errors.New("AgentUpgrade operation is already pending")

func defaultAgentUpgradePaths() agentUpgradePaths {
	const binaryDir = "/usr/local/lib/aks-flex-node"
	return agentUpgradePaths{
		BinaryPath:   "/usr/local/bin/aks-flex-node",
		BluePath:     filepath.Join(binaryDir, "aks-flex-node-blue"),
		GreenPath:    filepath.Join(binaryDir, "aks-flex-node-green"),
		CurrentPath:  filepath.Join(binaryDir, "aks-flex-node-current"),
		LastGoodPath: filepath.Join(binaryDir, "aks-flex-node-last-good"),
		SignalPath:   "/etc/aks-flex-node/agent-upgrade-signal.json",
	}
}

type agentUpgradeRequest struct {
	downloadURL string
	sha256      string
}

func parseAgentUpgradeRequest(parameters map[string]string) (agentUpgradeRequest, error) {
	request := agentUpgradeRequest{
		downloadURL: strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter]),
		sha256:      strings.TrimSpace(parameters[agentUpgradeSHA256Parameter]),
	}
	if request.downloadURL == "" {
		return agentUpgradeRequest{}, fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}
	if _, err := secureAgentInstallOptions(request.downloadURL, request.sha256); err != nil {
		return agentUpgradeRequest{}, err
	}
	return request, nil
}

type agentUpgradeSignal struct {
	OperationName            string `json:"operationName"`
	ActiveMachine            string `json:"activeMachine,omitempty"`
	CandidatePath            string `json:"candidatePath,omitempty"`
	InitiatingDaemonInstance string `json:"initiatingDaemonInstance,omitempty"`
	SwitchCommitted          bool   `json:"switchCommitted,omitempty"`
	RecoveryRequired         bool   `json:"recoveryRequired,omitempty"`
	Failure                  string `json:"failure,omitempty"`
}

type agentUpgradeSignalStore struct {
	path string
}

func (s agentUpgradeSignalStore) recordPending(operationName, activeMachine, daemonInstance string) error {
	return s.write(agentUpgradeSignal{
		OperationName:            operationName,
		ActiveMachine:            activeMachine,
		InitiatingDaemonInstance: daemonInstance,
	})
}

func (s agentUpgradeSignalStore) recordCandidate(candidatePath string) error {
	signal, err := s.read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.CandidatePath = candidatePath
	return s.write(*signal)
}

func (s agentUpgradeSignalStore) recordSwitchCommitted() error {
	signal, err := s.read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.SwitchCommitted = true
	return s.write(*signal)
}

func (s agentUpgradeSignalStore) recordFailure(message string) error {
	signal, err := s.read()
	if err != nil {
		return err
	}
	if signal == nil {
		return nil
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upgraded daemon failed to start; restored last-good binary"
	}
	signal.Failure = message
	signal.RecoveryRequired = true
	return s.write(*signal)
}

func (s agentUpgradeSignalStore) read() (*agentUpgradeSignal, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read AgentUpgrade signal: %w", err)
	}
	var signal agentUpgradeSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		return nil, fmt.Errorf("decode AgentUpgrade signal: %w", err)
	}
	signal.OperationName = strings.TrimSpace(signal.OperationName)
	signal.ActiveMachine = strings.TrimSpace(signal.ActiveMachine)
	signal.CandidatePath = strings.TrimSpace(signal.CandidatePath)
	signal.Failure = strings.TrimSpace(signal.Failure)
	if signal.OperationName == "" {
		return nil, fmt.Errorf("AgentUpgrade signal has no operation name")
	}
	if signal.ActiveMachine != "" && !validNspawnMachine(signal.ActiveMachine) {
		return nil, fmt.Errorf("AgentUpgrade signal has invalid active machine %q", signal.ActiveMachine)
	}
	return &signal, nil
}

func (s agentUpgradeSignalStore) write(signal agentUpgradeSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return fmt.Errorf("encode AgentUpgrade signal: %w", err)
	}
	if err := utilio.WriteFile(s.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write AgentUpgrade signal: %w", err)
	}
	return nil
}

func (s agentUpgradeSignalStore) clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove AgentUpgrade signal: %w", err)
	}
	return nil
}

type agentUpgradeExecutor interface {
	Acquire() (io.Closer, error)
	RecordPending(context.Context, string) error
	RetryRecovery(context.Context) error
	RecordFailure(string) error
	Stage(context.Context, agentUpgradeRequest) error
	Abort(context.Context) error
	Restart(context.Context) error
	WaitForRestart(context.Context) error
}

type agentUpgradeStateLoader interface {
	LoadState(context.Context) (*State, error)
}

type hostAgentUpgradeExecutor struct {
	log                    *slog.Logger
	paths                  agentUpgradePaths
	state                  agentUpgradeStateLoader
	signals                agentUpgradeSignalStore
	runSystemdRun          func(context.Context, ...string) error
	finishMachineOperation func(context.Context, client.Client, agentdaemon.MachineOperation, agentdaemon.MachineOperationResult[int64]) error
	runningExecutable      func() (string, error)
	instanceID             string
}

func newHostAgentUpgradeExecutor(log *slog.Logger, state agentUpgradeStateLoader) (*hostAgentUpgradeExecutor, error) {
	paths := defaultAgentUpgradePaths()
	instanceID, err := newDaemonInstanceID()
	if err != nil {
		return nil, fmt.Errorf("create daemon instance ID: %w", err)
	}
	return &hostAgentUpgradeExecutor{
		log:     log,
		paths:   paths,
		state:   state,
		signals: agentUpgradeSignalStore{path: paths.SignalPath},
		runSystemdRun: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
		},
		finishMachineOperation: agentdaemon.FinishMachineOperation,
		runningExecutable:      runningAgentExecutable,
		instanceID:             instanceID,
	}, nil
}

func newDaemonInstanceID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (e *hostAgentUpgradeExecutor) Acquire() (io.Closer, error) {
	return agentbinary.AcquireHostActivationLock(agentUpgradeLockPath)
}

func (e *hostAgentUpgradeExecutor) RecordPending(ctx context.Context, operationName string) error {
	existing, err := e.signals.read()
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.OperationName == operationName {
			return errAgentUpgradeAlreadyPending
		}
		return fmt.Errorf("another AgentUpgrade operation %q is pending", existing.OperationName)
	}
	state, err := e.state.LoadState(ctx)
	if err != nil {
		return fmt.Errorf("load daemon state for AgentUpgrade: %w", err)
	}
	if state == nil || !validNspawnMachine(state.ActiveMachine) {
		return fmt.Errorf("no valid active nspawn machine for AgentUpgrade")
	}
	if err := e.signals.recordPending(operationName, state.ActiveMachine, e.instanceID); err != nil {
		return err
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) RetryRecovery(ctx context.Context) error {
	signal, err := e.signals.read()
	if err != nil {
		return err
	}
	if signal == nil || !signal.RecoveryRequired {
		return nil
	}
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if err := e.Restart(cleanupCtx); err != nil {
		return fmt.Errorf("retry AgentUpgrade recovery restart: %w", err)
	}
	return e.WaitForRestart(ctx)
}

func (e *hostAgentUpgradeExecutor) RecordFailure(message string) error {
	return e.signals.recordFailure(message)
}

func (e *hostAgentUpgradeExecutor) Stage(ctx context.Context, request agentUpgradeRequest) error {
	if err := ensureAgentUpgradeLayout(ctx, e.log, e.paths); err != nil {
		return fmt.Errorf("initialize agent binary layout: %w", err)
	}
	current, err := resolvedExecutable(e.paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current agent binary: %w", err)
	}
	candidate := e.paths.BluePath
	if current == e.paths.BluePath {
		candidate = e.paths.GreenPath
	}
	if err := e.signals.recordCandidate(candidate); err != nil {
		return err
	}
	if err := installAndSwitchAgentBinary(ctx, e.log, request.downloadURL, request.sha256, e.paths); err != nil {
		return err
	}
	if err := e.signals.recordSwitchCommitted(); err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("record committed AgentUpgrade switch: %w", err))
	}

	signal, err := e.signals.read()
	if err != nil {
		return e.rollbackAfterStage(ctx, err)
	}
	if signal == nil || !validNspawnMachine(signal.ActiveMachine) {
		return e.rollbackAfterStage(ctx, fmt.Errorf("pending AgentUpgrade signal has no valid active machine"))
	}
	current, err = resolvedExecutable(e.paths.CurrentPath)
	if err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("resolve staged agent binary: %w", err))
	}
	if current != candidate {
		return e.rollbackAfterStage(ctx, fmt.Errorf("staged agent binary resolved to unexpected slot"))
	}
	if err := synchronizeNspawnAgentBinary(current, signal.ActiveMachine); err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("synchronize active nspawn agent binary: %w", err))
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) rollbackAfterStage(ctx context.Context, stageErr error) error {
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if rollbackErr := e.rollback(cleanupCtx); rollbackErr != nil {
		return fmt.Errorf("%w; rollback failed: %v", stageErr, rollbackErr)
	}
	return stageErr
}

func (e *hostAgentUpgradeExecutor) Abort(ctx context.Context) error {
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if err := e.rollback(cleanupCtx); err != nil {
		// Preserve the signal so startup recovery can retry the rollback.
		return err
	}
	return e.signals.clear()
}

func agentUpgradeCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (e *hostAgentUpgradeExecutor) rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signal, err := e.signals.read()
	if err != nil {
		return err
	}
	return rollbackAgentUpgradeFiles(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) WaitForRestart(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil
	case <-timer.C:
		return fmt.Errorf("timed out waiting for scheduled daemon restart")
	}
}

func (e *hostAgentUpgradeExecutor) Restart(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A direct self-restart can terminate the systemctl child before the
	// handler records a successful handoff. Schedule the restart in a separate
	// transient unit so this process returns while its durable signal is intact.
	unit := fmt.Sprintf("aks-flex-node-agent-upgrade-restart-%d", time.Now().UnixNano())
	return e.runSystemdRun(
		ctx,
		"--quiet",
		"--collect",
		"--unit="+unit,
		"--on-active=1s",
		"/usr/bin/systemctl",
		"restart",
		ServiceUnitName,
	)
}

func validNspawnMachine(machine string) bool {
	return machine == goalstates.NSpawnMachineKube1 || machine == goalstates.NSpawnMachineKube2
}

func synchronizeNspawnAgentBinary(sourcePath, machine string) error {
	if !validNspawnMachine(machine) {
		return fmt.Errorf("invalid nspawn machine %q", machine)
	}
	destination := filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
	if err := copyExecutable(sourcePath, destination); err != nil {
		return fmt.Errorf("copy agent binary to %s: %w", machine, err)
	}
	return nil
}

// RecoverAgentUpgrade records failure and restores both host and active nspawn
// binaries. It is invoked by the systemd recovery unit through last-good.
func RecoverAgentUpgrade(ctx context.Context, message string) error {
	paths := defaultAgentUpgradePaths()
	signals := agentUpgradeSignalStore{path: paths.SignalPath}
	if err := signals.recordFailure(message); err != nil {
		return err
	}
	signal, err := signals.read()
	if err != nil {
		return err
	}
	if err := rollbackAgentUpgradeFiles(paths, signal); err != nil {
		return err
	}
	return ctx.Err()
}

func publishAndClearAgentUpgradeSignal(ctx context.Context, log *slog.Logger, c client.Client, executor *hostAgentUpgradeExecutor) error {
	paths := executor.paths
	signals := executor.signals
	signal, err := signals.read()
	if err != nil {
		return err
	}
	if signal == nil {
		return nil
	}
	if signal.Failure == "" && signal.InitiatingDaemonInstance == executor.instanceID {
		// The initiating daemon must not consume its own signal while staging.
		return nil
	}

	if signal.Failure == "" {
		if validationErr := validateStartedAgentUpgrade(paths, signal); validationErr != nil {
			signal.Failure = validationErr.Error()
			candidateActive, activeErr := agentUpgradeCandidateIsActive(paths, signal)
			if activeErr != nil {
				signal.Failure = errors.Join(validationErr, activeErr).Error()
				signal.RecoveryRequired = true
			} else {
				signal.RecoveryRequired = candidateActive
			}
			if err := signals.write(*signal); err != nil {
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

		if err := rollbackAgentUpgradeFiles(paths, signal); err != nil {
			return fmt.Errorf("roll back failed AgentUpgrade: %w", err)
		}
	}

	if signal.RecoveryRequired {
		lastGood, err := resolvedExecutable(paths.LastGoodPath)
		if err != nil {
			return fmt.Errorf("resolve last-good agent for recovery restart: %w", err)
		}
		runningExecutable := executor.runningExecutable
		if runningExecutable == nil {
			runningExecutable = runningAgentExecutable
		}
		running, err := runningExecutable()
		if err != nil {
			return err
		}
		if running != lastGood {
			cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
			restartErr := executor.Restart(cleanupCtx)
			cancel()
			if restartErr != nil {
				return fmt.Errorf("restart last-good agent: %w", restartErr)
			}
			// Keep the signal and terminal status unpublished until the last-good
			// process confirms that it is running.
			return nil
		}
	}
	finishOperation := executor.finishMachineOperation
	if finishOperation == nil {
		finishOperation = agentdaemon.FinishMachineOperation
	}
	finishErr := finishOperation(ctx, c, agentdaemon.MachineOperation{Name: signal.OperationName}, result)
	if finishErr != nil {
		return fmt.Errorf("publish AgentUpgrade result: %w", finishErr)
	}
	if err := signals.clear(); err != nil {
		return err
	}
	log.Info("published AgentUpgrade result", "operation", signal.OperationName, "phase", result.Phase)
	return nil
}

func runningAgentExecutable() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve running agent executable: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve running agent executable symlinks: %w", err)
	}
	return resolved, nil
}

func wrapOptionalError(context string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", context, err)
}

func validateStartedAgentUpgrade(paths agentUpgradePaths, signal *agentUpgradeSignal) error {
	if signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return fmt.Errorf("AgentUpgrade was interrupted before selecting a candidate slot")
	}
	current, err := resolvedExecutable(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current upgraded agent binary: %w", err)
	}
	if current != signal.CandidatePath {
		return fmt.Errorf("AgentUpgrade was interrupted before switching the candidate binary")
	}
	if !validNspawnMachine(signal.ActiveMachine) {
		return fmt.Errorf("AgentUpgrade has no valid active nspawn machine")
	}
	nspawnPath := filepath.Join("/var/lib/machines", signal.ActiveMachine, "usr", "local", "bin", "aks-flex-node")
	equal, err := filesHaveEqualSHA256(current, nspawnPath)
	if err != nil {
		return fmt.Errorf("verify synchronized nspawn agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and nspawn agent binaries do not match")
	}
	return nil
}

func agentUpgradeCandidateIsActive(paths agentUpgradePaths, signal *agentUpgradeSignal) (bool, error) {
	if signal == nil || signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return false, nil
	}
	current, err := resolvedExecutable(paths.CurrentPath)
	if err != nil {
		return false, fmt.Errorf("resolve current agent binary for rollback: %w", err)
	}
	return current == signal.CandidatePath, nil
}

func rollbackAgentUpgradeFiles(paths agentUpgradePaths, signal *agentUpgradeSignal) error {
	if signal == nil {
		return nil
	}
	candidateActive, err := agentUpgradeCandidateIsActive(paths, signal)
	if err != nil {
		return err
	}
	if !signal.SwitchCommitted && !signal.RecoveryRequired && !candidateActive {
		return nil
	}
	lastGood, err := resolvedExecutable(paths.LastGoodPath)
	if err != nil {
		return fmt.Errorf("resolve last-good agent binary: %w", err)
	}
	if err := replaceSymlink(paths.CurrentPath, lastGood); err != nil {
		return fmt.Errorf("restore last-good agent binary: %w", err)
	}
	if validNspawnMachine(signal.ActiveMachine) {
		if err := synchronizeNspawnAgentBinary(lastGood, signal.ActiveMachine); err != nil {
			return err
		}
	}
	return nil
}

func filesHaveEqualSHA256(firstPath, secondPath string) (bool, error) {
	first, err := fileSHA256(firstPath)
	if err != nil {
		return false, err
	}
	second, err := fileSHA256(secondPath)
	if err != nil {
		return false, err
	}
	return first == second, nil
}

func fileSHA256(path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	file, err := os.Open(path) //nolint:gosec // fixed root-owned agent paths
	if err != nil {
		return digest, err
	}
	defer file.Close() //nolint:errcheck // read result is authoritative
	hasher := sha256.New()
	limited := io.LimitReader(file, agentUpgradeMaxBinaryBytes+1)
	n, err := io.Copy(hasher, limited)
	if err != nil {
		return digest, err
	}
	if n > agentUpgradeMaxBinaryBytes {
		return digest, fmt.Errorf("agent binary exceeds %d-byte limit", agentUpgradeMaxBinaryBytes)
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}
