package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeSHA256Parameter      = "sha256"
)

var errAgentUpgradeAlreadyPending = errors.New("AgentUpgrade operation is already pending")

type agentUpgradeRequest struct {
	downloadURL string
	sha256      string
}

// parseAgentUpgradeRequest reads an AgentUpgrade's parameters and checks what every agent requires
// of them: a download URL and a SHA-256. Which URLs an agent takes, an archive's or an image's, is
// its executor's to check; see agentUpgradeExecutor.Validate.
func parseAgentUpgradeRequest(parameters map[string]string) (agentUpgradeRequest, error) {
	request := agentUpgradeRequest{
		downloadURL: strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter]),
		sha256:      strings.TrimSpace(parameters[agentUpgradeSHA256Parameter]),
	}
	if request.downloadURL == "" {
		return agentUpgradeRequest{}, fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}
	if _, err := agentUpgradeDigest(request.sha256); err != nil {
		return agentUpgradeRequest{}, err
	}
	return request, nil
}

// agentUpgradeDigest returns an AgentUpgrade's SHA-256 in lower-case hex, without the optional
// "sha256:" prefix.
func agentUpgradeDigest(sha256Parameter string) (string, error) {
	if sha256Parameter == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeSHA256Parameter)
	}
	digest := strings.ToLower(strings.TrimPrefix(sha256Parameter, "sha256:"))
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("expected SHA-256 must be exactly 64 hexadecimal characters")
	}
	return digest, nil
}

type agentUpgradeExecutor interface {
	Acquire() (io.Closer, error)
	// Validate rejects a request this agent can't carry out: an image on a host, or a release
	// archive in the agent's container.
	Validate(agentUpgradeRequest) error
	RecordPending(context.Context, string) error
	RetryRecovery(context.Context) error
	RecordFailure(string) error
	Stage(context.Context, agentUpgradeRequest) error
	Abort(context.Context) error
	Restart(context.Context) error
}

// agentUpgradeHandoff is what the daemon that starts after an AgentUpgrade's restart needs to
// judge the upgrade and publish its result. On a host, the upgrade switches binaries; in the
// agent's container, images.
type agentUpgradeHandoff interface {
	signalStore() agentdeployment.UpgradeSignalStore
	daemonInstance() string
	// validateStarted returns why this daemon isn't the fully switched candidate, if it isn't.
	validateStarted(*agentdeployment.UpgradeSignal) error
	candidateActive(*agentdeployment.UpgradeSignal) (bool, error)
	rollbackSwitch(*agentdeployment.UpgradeSignal) error
	runningLastGood(*agentdeployment.UpgradeSignal) (bool, error)
	Restart(context.Context) error
	finish(context.Context, client.Client, agentdaemon.MachineOperation, agentdaemon.MachineOperationResult[int64]) error
}

// agentUpgrader carries out AgentUpgrades and publishes their results.
type agentUpgrader interface {
	agentUpgradeExecutor
	agentUpgradeHandoff
}

type agentUpgradeStateLoader interface {
	Load(context.Context) (*State, error)
}

type hostAgentUpgradeExecutor struct {
	log                    *slog.Logger
	paths                  agentdeployment.UpgradePaths
	state                  agentUpgradeStateLoader
	signals                agentdeployment.UpgradeSignalStore
	systemctl              func(context.Context, ...string) error
	runSystemdRun          func(context.Context, ...string) error
	finishMachineOperation func(context.Context, client.Client, agentdaemon.MachineOperation, agentdaemon.MachineOperationResult[int64]) error
	runningExecutable      func() (string, error)
	nspawnBinaryPath       func(string) string
	// guardLegacySeed runs before Stage switches anything; see the function
	// of the same name. Nil skips it.
	guardLegacySeed func() error
	instanceID      string
}

func newHostAgentUpgradeExecutor(log *slog.Logger, state agentUpgradeStateLoader) (*hostAgentUpgradeExecutor, error) {
	paths := agentdeployment.DefaultUpgradePaths()
	instanceID, err := newDaemonInstanceID()
	if err != nil {
		return nil, fmt.Errorf("create daemon instance ID: %w", err)
	}
	return &hostAgentUpgradeExecutor{
		log:     log,
		paths:   paths,
		state:   state,
		signals: agentdeployment.NewUpgradeSignalStore(paths.SignalPath),
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
		runSystemdRun: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
		},
		finishMachineOperation: agentdaemon.FinishMachineOperation,
		runningExecutable:      agentdeployment.RunningAgentExecutable,
		nspawnBinaryPath:       activeNspawnAgentBinaryPath,
		guardLegacySeed: func() error {
			return guardLegacySeed(log, hostroot.LegacyReleased, hostRootMoveUnfinishedHere, legacySeedPath, runningFrom)
		},
		instanceID: instanceID,
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
	return agentbinary.AcquireHostActivationLock(agentdeployment.UpgradeLockPath)
}

// Validate takes an archive's HTTP or HTTPS URL, and refuses an image's, as any other URL.
func (e *hostAgentUpgradeExecutor) Validate(request agentUpgradeRequest) error {
	_, err := agentdeployment.SecureInstallOptions(request.downloadURL, request.sha256)
	return err
}

func (e *hostAgentUpgradeExecutor) RecordPending(ctx context.Context, operationName string) error {
	return recordPendingAgentUpgrade(ctx, e.signals, e.state, operationName, e.instanceID)
}

func recordPendingAgentUpgrade(
	ctx context.Context,
	signals agentdeployment.UpgradeSignalStore,
	state agentUpgradeStateLoader,
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
	if loaded == nil || !agentdeployment.ValidNspawnMachine(loaded.ActiveMachine) {
		return fmt.Errorf("no valid active nspawn machine for AgentUpgrade")
	}
	if err := signals.RecordPending(operationName, loaded.ActiveMachine, instanceID); err != nil {
		return err
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) RetryRecovery(ctx context.Context) error {
	return retryAgentUpgradeRecovery(ctx, e.signals, e.Restart)
}

func retryAgentUpgradeRecovery(ctx context.Context, signals agentdeployment.UpgradeSignalStore, restart func(context.Context) error) error {
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if signal == nil || !signal.RecoveryRequired {
		return nil
	}
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if err := restart(cleanupCtx); err != nil {
		return fmt.Errorf("retry AgentUpgrade recovery restart: %w", err)
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) RecordFailure(message string) error {
	return e.signals.RecordFailure(message)
}

func (e *hostAgentUpgradeExecutor) Stage(ctx context.Context, request agentUpgradeRequest) error {
	if e.guardLegacySeed != nil {
		if err := e.guardLegacySeed(); err != nil {
			return err
		}
	}
	if err := agentdeployment.EnsureUpgradeLayout(ctx, e.log, e.paths); err != nil {
		return fmt.Errorf("initialize agent binary layout: %w", err)
	}
	current, err := agentdeployment.ResolveExecutable(e.paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current agent binary: %w", err)
	}
	candidate := e.paths.BluePath
	if current == e.paths.BluePath {
		candidate = e.paths.GreenPath
	}
	if err := e.signals.RecordCandidate(candidate); err != nil {
		return err
	}
	if err := agentdeployment.InstallAndSwitchBinary(ctx, e.log, request.downloadURL, request.sha256, e.paths); err != nil {
		return err
	}
	if err := e.signals.RecordSwitchCommitted(); err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("record committed AgentUpgrade switch: %w", err))
	}

	signal, err := e.signals.Read()
	if err != nil {
		return e.rollbackAfterStage(ctx, err)
	}
	if signal == nil || !agentdeployment.ValidNspawnMachine(signal.ActiveMachine) {
		return e.rollbackAfterStage(ctx, fmt.Errorf("pending AgentUpgrade signal has no valid active machine"))
	}
	current, err = agentdeployment.ResolveExecutable(e.paths.CurrentPath)
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
	return e.signals.Clear()
}

func agentUpgradeCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (e *hostAgentUpgradeExecutor) rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signal, err := e.signals.Read()
	if err != nil {
		return err
	}
	return rollbackAgentUpgradeFiles(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) Restart(ctx context.Context) error {
	return restartAgentAfterUpgrade(ctx, e.log, e.systemctl, e.runSystemdRun)
}

// restartAgentAfterUpgrade restarts the agent's unit a second from now, from a transient unit, as
// an AgentUpgrade's executor does, on a host and in the agent's container alike.
func restartAgentAfterUpgrade(
	ctx context.Context,
	log *slog.Logger,
	systemctl, runSystemdRun func(context.Context, ...string) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// systemd counts every start of the unit toward its start limit, deliberate restarts too, so
	// a few upgrades in a row would have the next restart refused, failing the unit, which
	// recovery takes for a failed upgrade, though the candidate never ran. The host root's move
	// restarts the daemon through this too, right after such a burst, perhaps: refused, its
	// restart would leave the agent stopped, as Restart= doesn't apply to a unit that hit its
	// limit, and recovery does nothing without a pending AgentUpgrade. reset-failed, naming the
	// unit, as with no name it resets every unit on the host, clears the count and leaves the
	// running unit as it is; the starts that follow, a failing candidate's included, count from
	// here. From systemd 255, the daemon-reload that rewriting the units runs no longer clears
	// it. Without it, the restart is still worth trying.
	if err := systemctl(ctx, "reset-failed", ServiceUnitName); err != nil {
		log.Warn("failed to reset the agent unit's start limit before restarting it",
			"unit", ServiceUnitName, "error", err)
	}
	// A direct self-restart can terminate the systemctl child before the
	// handler records a successful handoff. Schedule the restart in a separate
	// transient unit so this process returns while its durable signal is intact.
	return scheduleAgentUnitCommand(ctx, runSystemdRun, time.Second, "aks-flex-node-agent-upgrade-restart", "restart")
}

// scheduleAgentUnitCommand runs systemctl's command on the agent's unit from a transient timer,
// delay from now, in a transient unit named for the purpose.
func scheduleAgentUnitCommand(
	ctx context.Context,
	runSystemdRun func(context.Context, ...string) error,
	delay time.Duration,
	purpose, command string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A timer's AccuracySec= defaults to a minute, within which systemd may fire
	// it as late as it likes, so a restart due in a second took up to a minute.
	unit := fmt.Sprintf("%s-%d", purpose, time.Now().UnixNano())
	return runSystemdRun(
		ctx,
		"--quiet",
		"--collect",
		"--unit="+unit,
		fmt.Sprintf("--on-active=%dms", delay.Milliseconds()),
		"--timer-property=AccuracySec="+agentRestartTimerAccuracy,
		"/usr/bin/systemctl",
		command,
		ServiceUnitName,
	)
}

// agentRestartTimerAccuracy bounds how late systemd may fire the timer that restarts the agent.
const agentRestartTimerAccuracy = "100ms"

func synchronizeNspawnAgentBinary(sourcePath, machine string) error {
	if !agentdeployment.ValidNspawnMachine(machine) {
		return fmt.Errorf("invalid nspawn machine %q", machine)
	}
	destination := filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
	if err := agentdeployment.CopyExecutable(sourcePath, destination); err != nil {
		return fmt.Errorf("copy agent binary to %s: %w", machine, err)
	}
	return nil
}

// RecoverAgentUpgrade records failure and restores both host and active nspawn
// binaries. It is invoked by the systemd recovery unit through last-good. In
// the agent's container, it restores the last-good image and restarts the
// agent's unit, which the recovery script does on a host.
//
// On a host it recovers whatever the host root, since recovery is what brings
// a failed upgrade back. A host root that cannot be migrated is logged and left
// as it is, and the layout is the one this binary runs from: the recovery
// script runs the resolved last-good link, so that is the layout systemd uses.
func RecoverAgentUpgrade(ctx context.Context, message string) error {
	log := slog.Default()
	if agentdeployment.Containerized() {
		if err := recoverContainerAgentUpgrade(ctx, log, message,
			agentdeployment.NewUpgradeSignalStore(agentdeployment.DefaultUpgradePaths().SignalPath),
			agentImageEnvFile{path: agentImageSelectionPath},
			func(ctx context.Context, args ...string) error {
				return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
			}); err != nil {
			return err
		}
		return ctx.Err()
	}
	if err := agentdeployment.MigrateHostRoot(log); err != nil {
		log.Warn("recovering the agent without migrating the host root", "error", err)
	}
	paths := recoveryAgentUpgradePaths(agentdeployment.RunningAgentExecutable, hostroot.Resolve)
	signals := agentdeployment.NewUpgradeSignalStore(paths.SignalPath)
	if err := signals.RecordFailure(message); err != nil {
		return err
	}
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if err := rollbackAgentUpgradeFiles(paths, signal); err != nil {
		return err
	}
	return ctx.Err()
}

// recoveryAgentUpgradePaths returns the layout the running binary is a slot
// of, or the one under the resolved host root when it is not in a slot.
func recoveryAgentUpgradePaths(executable func() (string, error), resolve func() string) agentdeployment.UpgradePaths {
	if self, err := executable(); err == nil {
		root := filepath.Dir(filepath.Dir(filepath.Dir(self)))
		if paths := agentdeployment.UpgradePathsUnder(root); self == paths.BluePath || self == paths.GreenPath {
			return paths
		}
	}
	return agentdeployment.UpgradePathsUnder(resolve())
}

func publishAndClearAgentUpgradeSignal(ctx context.Context, log *slog.Logger, c client.Client, upgrade agentUpgradeHandoff) error {
	signals := upgrade.signalStore()
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if signal == nil {
		return nil
	}
	if signal.Failure == "" && signal.InitiatingDaemonInstance == upgrade.daemonInstance() {
		// The initiating daemon must not consume its own signal while staging.
		return nil
	}

	if signal.Failure == "" {
		if validationErr := upgrade.validateStarted(signal); validationErr != nil {
			signal.Failure = validationErr.Error()
			candidateActive, activeErr := upgrade.candidateActive(signal)
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

		if err := upgrade.rollbackSwitch(signal); err != nil {
			return fmt.Errorf("roll back failed AgentUpgrade: %w", err)
		}
	}

	if signal.RecoveryRequired {
		lastGood, err := upgrade.runningLastGood(signal)
		if err != nil {
			return err
		}
		if !lastGood {
			cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
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
	finishErr := upgrade.finish(ctx, c, agentdaemon.MachineOperation{Name: signal.OperationName}, result)
	if finishErr != nil {
		return fmt.Errorf("publish AgentUpgrade result: %w", finishErr)
	}
	if err := signals.Clear(); err != nil {
		return err
	}
	log.Info("published AgentUpgrade result", "operation", signal.OperationName, "phase", result.Phase)
	return nil
}

func (e *hostAgentUpgradeExecutor) signalStore() agentdeployment.UpgradeSignalStore { return e.signals }

var _ agentUpgrader = (*hostAgentUpgradeExecutor)(nil)

func (e *hostAgentUpgradeExecutor) daemonInstance() string { return e.instanceID }

func (e *hostAgentUpgradeExecutor) validateStarted(signal *agentdeployment.UpgradeSignal) error {
	nspawnBinaryPath := e.nspawnBinaryPath
	if nspawnBinaryPath == nil {
		nspawnBinaryPath = activeNspawnAgentBinaryPath
	}
	return validateStartedAgentUpgrade(e.paths, signal, nspawnBinaryPath)
}

func (e *hostAgentUpgradeExecutor) candidateActive(signal *agentdeployment.UpgradeSignal) (bool, error) {
	return agentUpgradeCandidateIsActive(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) rollbackSwitch(signal *agentdeployment.UpgradeSignal) error {
	return rollbackAgentUpgradeFiles(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) runningLastGood(*agentdeployment.UpgradeSignal) (bool, error) {
	lastGood, err := agentdeployment.ResolveExecutable(e.paths.LastGoodPath)
	if err != nil {
		return false, fmt.Errorf("resolve last-good agent for recovery restart: %w", err)
	}
	runningExecutable := e.runningExecutable
	if runningExecutable == nil {
		runningExecutable = agentdeployment.RunningAgentExecutable
	}
	running, err := runningExecutable()
	if err != nil {
		return false, err
	}
	return running == lastGood, nil
}

func (e *hostAgentUpgradeExecutor) finish(
	ctx context.Context,
	c client.Client,
	operation agentdaemon.MachineOperation,
	result agentdaemon.MachineOperationResult[int64],
) error {
	finishOperation := e.finishMachineOperation
	if finishOperation == nil {
		finishOperation = agentdaemon.FinishMachineOperation
	}
	return finishOperation(ctx, c, operation, result)
}

func wrapOptionalError(context string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", context, err)
}

func validateStartedAgentUpgrade(paths agentdeployment.UpgradePaths, signal *agentdeployment.UpgradeSignal, nspawnBinaryPath func(string) string) error {
	if signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return fmt.Errorf("AgentUpgrade was interrupted before selecting a candidate slot")
	}
	current, err := agentdeployment.ResolveExecutable(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current upgraded agent binary: %w", err)
	}
	if current != signal.CandidatePath {
		return fmt.Errorf("AgentUpgrade was interrupted before switching the candidate binary")
	}
	if !agentdeployment.ValidNspawnMachine(signal.ActiveMachine) {
		return fmt.Errorf("AgentUpgrade has no valid active nspawn machine")
	}
	nspawnPath := nspawnBinaryPath(signal.ActiveMachine)
	equal, err := filesHaveEqualSHA256(current, nspawnPath)
	if err != nil {
		return fmt.Errorf("verify synchronized nspawn agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and nspawn agent binaries do not match")
	}
	return nil
}

func agentUpgradeCandidateIsActive(paths agentdeployment.UpgradePaths, signal *agentdeployment.UpgradeSignal) (bool, error) {
	if signal == nil || signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return false, nil
	}
	current, err := agentdeployment.ResolveExecutable(paths.CurrentPath)
	if err != nil {
		return false, fmt.Errorf("resolve current agent binary for rollback: %w", err)
	}
	return current == signal.CandidatePath, nil
}

func activeNspawnAgentBinaryPath(machine string) string {
	return filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
}

func rollbackAgentUpgradeFiles(paths agentdeployment.UpgradePaths, signal *agentdeployment.UpgradeSignal) error {
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
	lastGood, err := agentdeployment.ResolveExecutable(paths.LastGoodPath)
	if err != nil {
		return fmt.Errorf("resolve last-good agent binary: %w", err)
	}
	if err := agentdeployment.ReplaceSymlink(paths.CurrentPath, lastGood); err != nil {
		return fmt.Errorf("restore last-good agent binary: %w", err)
	}
	if agentdeployment.ValidNspawnMachine(signal.ActiveMachine) {
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
	limited := io.LimitReader(file, agentdeployment.UpgradeMaxBinaryBytes+1)
	n, err := io.Copy(hasher, limited)
	if err != nil {
		return digest, err
	}
	if n > agentdeployment.UpgradeMaxBinaryBytes {
		return digest, fmt.Errorf("agent binary exceeds %d-byte limit", agentdeployment.UpgradeMaxBinaryBytes)
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}
