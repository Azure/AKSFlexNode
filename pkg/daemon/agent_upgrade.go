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

	"github.com/distribution/reference"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentUpgradeSHA256Parameter      = "sha256"
	// agentUpgradeImageParameter names the image an AgentUpgrade of the agent in its container
	// switches to, in place of a release archive.
	agentUpgradeImageParameter = "image"
)

var errAgentUpgradeAlreadyPending = errors.New("AgentUpgrade operation is already pending")

// defaultAgentUpgradePaths returns the host-side agent upgrade layout under the
// resolved host root. Callers that change the host migrate it first; see
// MigrateHostRoot.
func defaultAgentUpgradePaths() agentUpgradePaths {
	return agentUpgradePathsUnder(hostroot.Resolve())
}

type agentUpgradeRequest struct {
	downloadURL string
	sha256      string
	image       string
}

func parseAgentUpgradeRequest(parameters map[string]string) (agentUpgradeRequest, error) {
	request := agentUpgradeRequest{
		downloadURL: strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter]),
		sha256:      strings.TrimSpace(parameters[agentUpgradeSHA256Parameter]),
		image:       strings.TrimSpace(parameters[agentUpgradeImageParameter]),
	}
	if request.image != "" {
		if request.downloadURL != "" || request.sha256 != "" {
			return agentUpgradeRequest{}, fmt.Errorf("parameter %q excludes %q and %q",
				agentUpgradeImageParameter, agentUpgradeDownloadURLParameter, agentUpgradeSHA256Parameter)
		}
		if err := validateAgentImage(request.image); err != nil {
			return agentUpgradeRequest{}, err
		}
		return request, nil
	}
	if request.downloadURL == "" {
		return agentUpgradeRequest{}, fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}
	if _, err := secureAgentInstallOptions(request.downloadURL, request.sha256); err != nil {
		return agentUpgradeRequest{}, err
	}
	return request, nil
}

// validateAgentImage accepts a fully qualified image reference pinned by digest, with or without
// a tag, as ctr pulls it. As an archive needs its SHA-256, an image needs its digest: ctr verifies
// the content it pulls against it, so the agent never runs an unverified candidate, not even to
// check its version.
func validateAgentImage(image string) error {
	named, err := reference.ParseNamed(image)
	if err != nil {
		return fmt.Errorf("parameter %q: invalid image reference: %w", agentUpgradeImageParameter, err)
	}
	if _, digested := named.(reference.Digested); !digested {
		return fmt.Errorf("parameter %q: image reference %q has no digest", agentUpgradeImageParameter, image)
	}
	return nil
}

type agentUpgradeSignal struct {
	OperationName            string `json:"operationName"`
	ActiveMachine            string `json:"activeMachine,omitempty"`
	CandidatePath            string `json:"candidatePath,omitempty"`
	InitiatingDaemonInstance string `json:"initiatingDaemonInstance,omitempty"`
	SwitchCommitted          bool   `json:"switchCommitted,omitempty"`
	RecoveryRequired         bool   `json:"recoveryRequired,omitempty"`
	Failure                  string `json:"failure,omitempty"`
	// In the agent's container, the image an upgrade switches to and the one it switches from.
	CandidateImage string `json:"candidateImage,omitempty"`
	PreviousImage  string `json:"previousImage,omitempty"`
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

func (s agentUpgradeSignalStore) recordCandidateImage(candidate, previous string) error {
	signal, err := s.read()
	if err != nil {
		return err
	}
	if signal == nil {
		return fmt.Errorf("no pending AgentUpgrade signal")
	}
	signal.CandidateImage = candidate
	signal.PreviousImage = previous
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
	signalStore() agentUpgradeSignalStore
	daemonInstance() string
	// validateStarted returns why this daemon isn't the fully switched candidate, if it isn't.
	validateStarted(*agentUpgradeSignal) error
	candidateActive(*agentUpgradeSignal) (bool, error)
	rollbackSwitch(*agentUpgradeSignal) error
	runningLastGood(*agentUpgradeSignal) (bool, error)
	Restart(context.Context) error
	finish(context.Context, client.Client, agentdaemon.MachineOperation, agentdaemon.MachineOperationResult[int64]) error
}

// agentUpgrader carries out AgentUpgrades and publishes their results.
type agentUpgrader interface {
	agentUpgradeExecutor
	agentUpgradeHandoff
}

type agentUpgradeStateLoader interface {
	LoadState(context.Context) (*State, error)
}

type hostAgentUpgradeExecutor struct {
	log                    *slog.Logger
	paths                  agentUpgradePaths
	state                  agentUpgradeStateLoader
	signals                agentUpgradeSignalStore
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
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
		runSystemdRun: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
		},
		finishMachineOperation: agentdaemon.FinishMachineOperation,
		runningExecutable:      runningAgentExecutable,
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
	return agentbinary.AcquireHostActivationLock(agentUpgradeLockPath)
}

func (e *hostAgentUpgradeExecutor) Validate(request agentUpgradeRequest) error {
	if request.image != "" {
		return fmt.Errorf("parameter %q is for the agent in its container; on a host, AgentUpgrade takes %q",
			agentUpgradeImageParameter, agentUpgradeDownloadURLParameter)
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) RecordPending(ctx context.Context, operationName string) error {
	return recordPendingAgentUpgrade(ctx, e.signals, e.state, operationName, e.instanceID)
}

func recordPendingAgentUpgrade(
	ctx context.Context,
	signals agentUpgradeSignalStore,
	state agentUpgradeStateLoader,
	operationName, instanceID string,
) error {
	existing, err := signals.read()
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.OperationName == operationName {
			return errAgentUpgradeAlreadyPending
		}
		return fmt.Errorf("another AgentUpgrade operation %q is pending", existing.OperationName)
	}
	loaded, err := state.LoadState(ctx)
	if err != nil {
		return fmt.Errorf("load daemon state for AgentUpgrade: %w", err)
	}
	if loaded == nil || !validNspawnMachine(loaded.ActiveMachine) {
		return fmt.Errorf("no valid active nspawn machine for AgentUpgrade")
	}
	if err := signals.recordPending(operationName, loaded.ActiveMachine, instanceID); err != nil {
		return err
	}
	return nil
}

func (e *hostAgentUpgradeExecutor) RetryRecovery(ctx context.Context) error {
	return retryAgentUpgradeRecovery(ctx, e.signals, e.Restart)
}

func retryAgentUpgradeRecovery(ctx context.Context, signals agentUpgradeSignalStore, restart func(context.Context) error) error {
	signal, err := signals.read()
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
	return e.signals.recordFailure(message)
}

func (e *hostAgentUpgradeExecutor) Stage(ctx context.Context, request agentUpgradeRequest) error {
	if e.guardLegacySeed != nil {
		if err := e.guardLegacySeed(); err != nil {
			return err
		}
	}
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

func (e *hostAgentUpgradeExecutor) Restart(ctx context.Context) error {
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
	if err := e.systemctl(ctx, "reset-failed", ServiceUnitName); err != nil {
		e.log.Warn("failed to reset the agent unit's start limit before restarting it",
			"unit", ServiceUnitName, "error", err)
	}
	return scheduleAgentRestart(ctx, e.runSystemdRun, time.Second)
}

// scheduleAgentRestart restarts the agent's unit after the delay, from a transient unit.
func scheduleAgentRestart(ctx context.Context, runSystemdRun func(context.Context, ...string) error, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// A direct self-restart can terminate the systemctl child before the
	// handler records a successful handoff. Schedule the restart in a separate
	// transient unit so this process returns while its durable signal is intact.
	// A timer's AccuracySec= defaults to a minute, within which systemd may fire
	// it as late as it likes, so a restart due in a second took up to a minute.
	unit := fmt.Sprintf("aks-flex-node-agent-upgrade-restart-%d", time.Now().UnixNano())
	return runSystemdRun(
		ctx,
		"--quiet",
		"--collect",
		"--unit="+unit,
		fmt.Sprintf("--on-active=%dms", delay.Milliseconds()),
		"--timer-property=AccuracySec="+agentRestartTimerAccuracy,
		"/usr/bin/systemctl",
		"restart",
		ServiceUnitName,
	)
}

// agentRestartTimerAccuracy bounds how late systemd may fire the timer that restarts the agent.
const agentRestartTimerAccuracy = "100ms"

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
	if Containerized() {
		if err := recoverContainerAgentUpgrade(ctx, log, message,
			agentUpgradeSignalStore{path: defaultAgentUpgradePaths().SignalPath},
			agentImageEnvFile{path: agentImageSelectionPath},
			func(ctx context.Context, args ...string) error {
				return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
			},
			func(ctx context.Context) (string, error) {
				state, err := utilexec.OutputCmd(ctx, log, "systemctl", "show", "-p", "ActiveState", "--value", ServiceUnitName)
				return strings.TrimSpace(state), err
			},
			func(ctx context.Context, args ...string) error {
				return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
			}); err != nil {
			return err
		}
		return ctx.Err()
	}
	if err := MigrateHostRoot(log); err != nil {
		log.Warn("recovering the agent without migrating the host root", "error", err)
	}
	paths := recoveryAgentUpgradePaths(runningAgentExecutable, hostroot.Resolve)
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

// recoveryAgentUpgradePaths returns the layout the running binary is a slot
// of, or the one under the resolved host root when it is not in a slot.
func recoveryAgentUpgradePaths(executable func() (string, error), resolve func() string) agentUpgradePaths {
	if self, err := executable(); err == nil {
		root := filepath.Dir(filepath.Dir(filepath.Dir(self)))
		if paths := agentUpgradePathsUnder(root); self == paths.BluePath || self == paths.GreenPath {
			return paths
		}
	}
	return agentUpgradePathsUnder(resolve())
}

func publishAndClearAgentUpgradeSignal(ctx context.Context, log *slog.Logger, c client.Client, upgrade agentUpgradeHandoff) error {
	signals := upgrade.signalStore()
	signal, err := signals.read()
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
	if err := signals.clear(); err != nil {
		return err
	}
	log.Info("published AgentUpgrade result", "operation", signal.OperationName, "phase", result.Phase)
	return nil
}

func (e *hostAgentUpgradeExecutor) signalStore() agentUpgradeSignalStore { return e.signals }

var _ agentUpgrader = (*hostAgentUpgradeExecutor)(nil)

func (e *hostAgentUpgradeExecutor) daemonInstance() string { return e.instanceID }

func (e *hostAgentUpgradeExecutor) validateStarted(signal *agentUpgradeSignal) error {
	nspawnBinaryPath := e.nspawnBinaryPath
	if nspawnBinaryPath == nil {
		nspawnBinaryPath = activeNspawnAgentBinaryPath
	}
	return validateStartedAgentUpgrade(e.paths, signal, nspawnBinaryPath)
}

func (e *hostAgentUpgradeExecutor) candidateActive(signal *agentUpgradeSignal) (bool, error) {
	return agentUpgradeCandidateIsActive(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) rollbackSwitch(signal *agentUpgradeSignal) error {
	return rollbackAgentUpgradeFiles(e.paths, signal)
}

func (e *hostAgentUpgradeExecutor) runningLastGood(*agentUpgradeSignal) (bool, error) {
	lastGood, err := resolvedExecutable(e.paths.LastGoodPath)
	if err != nil {
		return false, fmt.Errorf("resolve last-good agent for recovery restart: %w", err)
	}
	runningExecutable := e.runningExecutable
	if runningExecutable == nil {
		runningExecutable = runningAgentExecutable
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

func validateStartedAgentUpgrade(paths agentUpgradePaths, signal *agentUpgradeSignal, nspawnBinaryPath func(string) string) error {
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

func activeNspawnAgentBinaryPath(machine string) string {
	return filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
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
