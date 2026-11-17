package agentdeployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

const (
	UpgradeDownloadURLParameter = "downloadURL"
	UpgradeSHA256Parameter      = "sha256"
)

// UpgradeRequest contains resolved, validated agent upgrade inputs.
type UpgradeRequest struct {
	DownloadURL string
	SHA256      string
}

// UpgradeExecutor performs deployment mechanics; daemon owns state and result publication.
type UpgradeExecutor interface {
	Acquire() (io.Closer, error)
	Validate(UpgradeRequest) error
	RetryRecovery(context.Context) error
	RecordFailure(string) error
	Stage(context.Context, UpgradeRequest) error
	Abort(context.Context) error
	Restart(context.Context) error
	SignalStore() UpgradeSignalStore
	ValidateStarted(*UpgradeSignal) error
	CandidateActive(*UpgradeSignal) (bool, error)
	RollbackSwitch(*UpgradeSignal) error
	RunningLastGood(*UpgradeSignal) (bool, error)
}

// UpgradeDigest returns an AgentUpgrade's SHA-256 in lower-case hex, without the optional
// "sha256:" prefix.
func UpgradeDigest(sha256Parameter string) (string, error) {
	if sha256Parameter == "" {
		return "", fmt.Errorf("missing required parameter %q", UpgradeSHA256Parameter)
	}
	digest := strings.ToLower(strings.TrimPrefix(sha256Parameter, "sha256:"))
	if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("expected SHA-256 must be exactly 64 hexadecimal characters")
	}
	return digest, nil
}

type HostUpgrade struct {
	log               *slog.Logger
	paths             UpgradePaths
	signals           UpgradeSignalStore
	systemctl         func(context.Context, ...string) error
	runSystemdRun     func(context.Context, ...string) error
	runningExecutable func() (string, error)
	mode              isolation.Mode
	// helperPaths are concrete destinations resolved through the worker runtime.
	// None has no guest helper and supplies no destinations.
	helperPaths map[string]string
	// guardLegacySeed runs before Stage switches anything; see the function
	// of the same name. Nil skips it.
	guardLegacySeed func() error
}

func NewHostUpgrade(log *slog.Logger, mode isolation.Mode, helperPaths map[string]string) *HostUpgrade {
	paths := DefaultUpgradePaths()
	return &HostUpgrade{
		log:     log,
		paths:   paths,
		signals: NewUpgradeSignalStore(paths.SignalPath),
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
		runSystemdRun: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
		},
		runningExecutable: runningAgentExecutable,
		mode:              mode,
		helperPaths:       helperPaths,
		guardLegacySeed: func() error {
			return guardUpgradeLegacySeed(log)
		},
	}
}

// Paths is the layout resolved when this process constructed the executor.
func (e *HostUpgrade) Paths() UpgradePaths {
	return e.paths
}

func (e *HostUpgrade) Acquire() (io.Closer, error) {
	return agentbinary.AcquireHostActivationLock(upgradeLockPath)
}

// Validate takes an archive's HTTP or HTTPS URL, and refuses an image's, as any other URL.
func (e *HostUpgrade) Validate(request UpgradeRequest) error {
	_, err := secureInstallOptions(request.DownloadURL, request.SHA256, e.mode)
	return err
}

func (e *HostUpgrade) RetryRecovery(ctx context.Context) error {
	return retryAgentUpgradeRecovery(ctx, e.signals, e.Restart)
}

func retryAgentUpgradeRecovery(ctx context.Context, signals UpgradeSignalStore, restart func(context.Context) error) error {
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if signal == nil || !signal.RecoveryRequired {
		return nil
	}
	cleanupCtx, cancel := UpgradeCleanupContext(ctx)
	defer cancel()
	if err := restart(cleanupCtx); err != nil {
		return fmt.Errorf("retry AgentUpgrade recovery restart: %w", err)
	}
	return nil
}

func (e *HostUpgrade) RecordFailure(message string) error {
	return e.signals.RecordFailure(message)
}

func (e *HostUpgrade) Stage(ctx context.Context, request UpgradeRequest) error {
	if e.guardLegacySeed != nil {
		if err := e.guardLegacySeed(); err != nil {
			return err
		}
	}
	if err := ensureUpgradeLayout(ctx, e.log, e.paths); err != nil {
		return fmt.Errorf("initialize agent binary layout: %w", err)
	}
	current, err := resolveExecutable(e.paths.CurrentPath)
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
	if err := installAndSwitchBinary(ctx, e.log, request.DownloadURL, request.SHA256, e.paths, e.mode); err != nil {
		return err
	}
	if err := e.signals.recordSwitchCommitted(); err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("record committed AgentUpgrade switch: %w", err))
	}

	signal, err := e.signals.Read()
	if err != nil {
		return e.rollbackAfterStage(ctx, err)
	}
	if signal == nil || !validNspawnMachine(signal.ActiveMachine) {
		return e.rollbackAfterStage(ctx, fmt.Errorf("pending AgentUpgrade signal has no valid active machine"))
	}
	current, err = resolveExecutable(e.paths.CurrentPath)
	if err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("resolve staged agent binary: %w", err))
	}
	if current != candidate {
		return e.rollbackAfterStage(ctx, fmt.Errorf("staged agent binary resolved to unexpected slot"))
	}
	if e.mode != isolation.ModeNone {
		if err := copyExecutable(current, e.helperPaths[signal.ActiveMachine]); err != nil {
			return e.rollbackAfterStage(ctx, fmt.Errorf("synchronize worker credential helper: %w", err))
		}
	}
	return nil
}

func (e *HostUpgrade) rollbackAfterStage(ctx context.Context, stageErr error) error {
	cleanupCtx, cancel := UpgradeCleanupContext(ctx)
	defer cancel()
	if rollbackErr := e.rollback(cleanupCtx); rollbackErr != nil {
		return fmt.Errorf("%w; rollback failed: %v", stageErr, rollbackErr)
	}
	return stageErr
}

func (e *HostUpgrade) Abort(ctx context.Context) error {
	cleanupCtx, cancel := UpgradeCleanupContext(ctx)
	defer cancel()
	if err := e.rollback(cleanupCtx); err != nil {
		// Preserve the signal so startup recovery can retry the rollback.
		return err
	}
	return e.signals.Clear()
}

func UpgradeCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func (e *HostUpgrade) rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signal, err := e.signals.Read()
	if err != nil {
		return err
	}
	return rollbackAgentUpgradeFiles(e.paths, signal, e.mode, e.helperPaths)
}

func (e *HostUpgrade) Restart(ctx context.Context) error {
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

// RecoverAgentUpgrade records failure and restores both host and active worker
// binaries. It is invoked by the systemd recovery unit through last-good. In
// the agent's container, it restores the last-good image and restarts the
// agent's unit, which the recovery script does on a host.
//
// On a host it recovers whatever the host root, since recovery is what brings
// a failed upgrade back. A host root that cannot be migrated is logged and left
// as it is, and the layout is the one this binary runs from: the recovery
// script runs the resolved last-good link, so that is the layout systemd uses.
func RecoverAgentUpgrade(ctx context.Context, log *slog.Logger, message string, mode isolation.Mode, helperPaths map[string]string) error {
	if log == nil {
		return fmt.Errorf("logger is nil")
	}

	if Containerized() {
		if err := recoverContainerAgentUpgrade(ctx, log, message,
			NewUpgradeSignalStore(DefaultUpgradePaths().SignalPath),
			agentImageEnvFile{path: agentImageSelectionPath},
			func(ctx context.Context, args ...string) error {
				return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
			}); err != nil {
			return err
		}
		return ctx.Err()
	}
	if err := MigrateHostRoot(log); err != nil {
		log.Warn("recovering the agent without migrating the host root", "error", err)
	}
	paths := recoveryAgentUpgradePaths(runningAgentExecutable, hostroot.Resolve)
	signals := NewUpgradeSignalStore(paths.SignalPath)
	if err := signals.RecordFailure(message); err != nil {
		return err
	}
	signal, err := signals.Read()
	if err != nil {
		return err
	}
	if err := rollbackAgentUpgradeFiles(paths, signal, mode, helperPaths); err != nil {
		return err
	}
	return ctx.Err()
}

// recoveryAgentUpgradePaths returns the layout the running binary is a slot
// of, or the one under the resolved host root when it is not in a slot.
func recoveryAgentUpgradePaths(executable func() (string, error), resolve func() string) UpgradePaths {
	if self, err := executable(); err == nil {
		root := filepath.Dir(filepath.Dir(filepath.Dir(self)))
		if paths := UpgradePathsUnder(root); self == paths.BluePath || self == paths.GreenPath {
			return paths
		}
	}
	return UpgradePathsUnder(resolve())
}

func (e *HostUpgrade) SignalStore() UpgradeSignalStore { return e.signals }

var _ UpgradeExecutor = (*HostUpgrade)(nil)

func (e *HostUpgrade) ValidateStarted(signal *UpgradeSignal) error {
	return validateStartedAgentUpgrade(e.paths, signal, e.mode, e.helperPaths)
}

func (e *HostUpgrade) CandidateActive(signal *UpgradeSignal) (bool, error) {
	return agentUpgradeCandidateIsActive(e.paths, signal)
}

func (e *HostUpgrade) RollbackSwitch(signal *UpgradeSignal) error {
	return rollbackAgentUpgradeFiles(e.paths, signal, e.mode, e.helperPaths)
}

func (e *HostUpgrade) RunningLastGood(*UpgradeSignal) (bool, error) {
	lastGood, err := resolveExecutable(e.paths.LastGoodPath)
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

func validateStartedAgentUpgrade(paths UpgradePaths, signal *UpgradeSignal, mode isolation.Mode, helperPaths map[string]string) error {
	if signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return fmt.Errorf("AgentUpgrade was interrupted before selecting a candidate slot")
	}
	current, err := resolveExecutable(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current upgraded agent binary: %w", err)
	}
	if current != signal.CandidatePath {
		return fmt.Errorf("AgentUpgrade was interrupted before switching the candidate binary")
	}
	if !validNspawnMachine(signal.ActiveMachine) {
		return fmt.Errorf("AgentUpgrade has no valid active worker")
	}
	if mode == isolation.ModeNone {
		return nil
	}

	equal, err := filesHaveEqualSHA256(current, helperPaths[signal.ActiveMachine])
	if err != nil {
		return fmt.Errorf("verify synchronized worker credential helper: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and worker agent binaries do not match")
	}
	return nil
}

func agentUpgradeCandidateIsActive(paths UpgradePaths, signal *UpgradeSignal) (bool, error) {
	if signal == nil || signal.CandidatePath != paths.BluePath && signal.CandidatePath != paths.GreenPath {
		return false, nil
	}
	current, err := resolveExecutable(paths.CurrentPath)
	if err != nil {
		return false, fmt.Errorf("resolve current agent binary for rollback: %w", err)
	}
	return current == signal.CandidatePath, nil
}

func activeNspawnAgentBinaryPath(machine string) string {
	return filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
}

func rollbackAgentUpgradeFiles(paths UpgradePaths, signal *UpgradeSignal, mode isolation.Mode, helperPaths map[string]string) error {
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
	lastGood, err := resolveExecutable(paths.LastGoodPath)
	if err != nil {
		return fmt.Errorf("resolve last-good agent binary: %w", err)
	}
	if err := replaceSymlink(paths.CurrentPath, lastGood); err != nil {
		return fmt.Errorf("restore last-good agent binary: %w", err)
	}
	if mode != isolation.ModeNone && validNspawnMachine(signal.ActiveMachine) {
		if err := copyExecutable(lastGood, helperPaths[signal.ActiveMachine]); err != nil {
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
	limited := io.LimitReader(file, upgradeMaxBinaryBytes+1)
	n, err := io.Copy(hasher, limited)
	if err != nil {
		return digest, err
	}
	if n > upgradeMaxBinaryBytes {
		return digest, fmt.Errorf("agent binary exceeds %d-byte limit", upgradeMaxBinaryBytes)
	}
	copy(digest[:], hasher.Sum(nil))
	return digest, nil
}
