package daemon

import (
	"bytes"
	"context"
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
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

// In its container, the agent upgrades by switching images rather than binaries. An AgentUpgrade
// names the image; the agent pulls it, checks that its agent runs, and selects it in
// agentImageSelectionPath, which the agent's unit reads, before restarting on it. The rest is
// the host's: the durable signal, the restart from a transient unit, and the new daemon's
// judgment of the upgrade and its result, with the recovery unit restoring the last-good image
// if the new one keeps failing.
const (
	// agentImageSelectionPath is the agent's choice of its own image, and the last good one, a
	// systemd environment file that the agent's unit and its recovery unit read after the host's
	// provisioning. Without it, they run the image the provisioning names.
	agentImageSelectionPath = "/etc/aks-flex-node/agent-image.env"
	agentImageEnv           = "AKS_FLEX_NODE_IMAGE"
	agentLastGoodImageEnv   = "AKS_FLEX_NODE_LAST_GOOD_IMAGE"
	// Flags for pulling and running the agent's images, which its unit passes in from the host's
	// provisioning.
	agentPullArgsEnv            = "AKS_FLEX_NODE_PULL_ARGS"
	agentSnapshotterPullArgsEnv = "AKS_FLEX_NODE_SNAPSHOTTER_PULL_ARGS"
	agentSnapshotterRunArgsEnv  = "AKS_FLEX_NODE_SNAPSHOTTER_RUN_ARGS"
	agentContainerdNamespace    = "aks-flex"
	// agentInstallSettingsPath is the host's provisioning's settings, which name the install's
	// image as AKS_FLEX_NODE_IMAGE. The entrypoint mounts them read-only.
	agentInstallSettingsPath = "/etc/sysconfig/aks-flex-node-container"
	// agentUnitStartLimitInterval is the StartLimitIntervalSec of the agent's unit in its container.
	agentUnitStartLimitInterval = 5 * time.Minute
)

type agentImageSelection struct {
	path string
}

func (s agentImageSelection) read() (current, lastGood string, err error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read the agent's image selection: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		// As systemd reads it, a value may be quoted.
		value = strings.Trim(value, `"'`)
		switch name {
		case agentImageEnv:
			current = value
		case agentLastGoodImageEnv:
			lastGood = value
		}
	}
	return current, lastGood, nil
}

// write selects the agent's image. Both are validated image references, which need no quoting.
func (s agentImageSelection) write(current, lastGood string) error {
	content := fmt.Sprintf("%s=%s\n%s=%s\n", agentImageEnv, current, agentLastGoodImageEnv, lastGood)
	if err := utilio.WriteFile(s.path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write the agent's image selection: %w", err)
	}
	return nil
}

type containerAgentUpgradeExecutor struct {
	log     *slog.Logger
	state   agentUpgradeStateLoader
	signals agentUpgradeSignalStore
	images  agentImageSelection
	// runningImage is the image this daemon runs, which its unit passes in.
	runningImage           string
	pullArgs               []string
	runArgs                []string
	ctr                    func(context.Context, ...string) error
	runSystemdRun          func(context.Context, ...string) error
	finishMachineOperation func(context.Context, client.Client, agentdaemon.MachineOperation, agentdaemon.MachineOperationResult[int64]) error
	runningExecutable      func() (string, error)
	nspawnBinaryPath       func(string) string
	instanceID             string
}

func newContainerAgentUpgradeExecutor(log *slog.Logger, state agentUpgradeStateLoader) (*containerAgentUpgradeExecutor, error) {
	instanceID, err := newDaemonInstanceID()
	if err != nil {
		return nil, fmt.Errorf("create daemon instance ID: %w", err)
	}
	return &containerAgentUpgradeExecutor{
		log:          log,
		state:        state,
		signals:      agentUpgradeSignalStore{path: defaultAgentUpgradePaths().SignalPath},
		images:       agentImageSelection{path: agentImageSelectionPath},
		runningImage: os.Getenv(agentImageEnv),
		pullArgs: append(strings.Fields(os.Getenv(agentSnapshotterPullArgsEnv)),
			strings.Fields(os.Getenv(agentPullArgsEnv))...),
		runArgs: strings.Fields(os.Getenv(agentSnapshotterRunArgsEnv)),
		ctr: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Ctr(), args...)
		},
		runSystemdRun: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.SystemdRun(), args...)
		},
		finishMachineOperation: agentdaemon.FinishMachineOperation,
		runningExecutable:      runningAgentExecutable,
		nspawnBinaryPath:       activeNspawnAgentBinaryPath,
		instanceID:             instanceID,
	}, nil
}

var _ agentUpgrader = (*containerAgentUpgradeExecutor)(nil)

func (e *containerAgentUpgradeExecutor) Acquire() (io.Closer, error) {
	return agentbinary.AcquireHostActivationLock(agentUpgradeLockPath)
}

func (e *containerAgentUpgradeExecutor) Validate(request agentUpgradeRequest) error {
	if request.image == "" {
		return fmt.Errorf("the agent runs in its container, where AgentUpgrade takes %q rather than %q",
			agentUpgradeImageParameter, agentUpgradeDownloadURLParameter)
	}
	return nil
}

func (e *containerAgentUpgradeExecutor) RecordPending(ctx context.Context, operationName string) error {
	return recordPendingAgentUpgrade(ctx, e.signals, e.state, operationName, e.instanceID)
}

func (e *containerAgentUpgradeExecutor) RetryRecovery(ctx context.Context) error {
	return retryAgentUpgradeRecovery(ctx, e.signals, e.Restart)
}

func (e *containerAgentUpgradeExecutor) RecordFailure(message string) error {
	return e.signals.recordFailure(message)
}

func (e *containerAgentUpgradeExecutor) Stage(ctx context.Context, request agentUpgradeRequest) error {
	previous, err := e.currentImage()
	if err != nil {
		return err
	}
	if err := e.signals.recordCandidateImage(request.image, previous); err != nil {
		return err
	}
	pull := append([]string{"--namespace", agentContainerdNamespace, "image", "pull"}, e.pullArgs...)
	if err := e.ctr(ctx, append(pull, request.image)...); err != nil {
		return fmt.Errorf("pull %s: %w", request.image, err)
	}
	// As on a host, the candidate's agent must run before the agent switches to it.
	check := append([]string{"--namespace", agentContainerdNamespace, "run", "--rm"}, e.runArgs...)
	check = append(check, request.image, fmt.Sprintf("aks-flex-node-upgrade-check-%d", time.Now().UnixNano()),
		"aks-flex-node", "version")
	if err := e.ctr(ctx, check...); err != nil {
		return fmt.Errorf("run the agent in %s: %w", request.image, err)
	}
	if err := e.images.write(request.image, previous); err != nil {
		return e.rollbackAfterStage(ctx, err)
	}
	if err := e.signals.recordSwitchCommitted(); err != nil {
		return e.rollbackAfterStage(ctx, fmt.Errorf("record committed AgentUpgrade switch: %w", err))
	}
	e.log.Info("selected the agent's upgraded image", "image", request.image, "previous", previous)
	return nil
}

// currentImage is the image the agent's unit runs: the selected one, or the one the host's
// provisioning names, which the unit passes in.
func (e *containerAgentUpgradeExecutor) currentImage() (string, error) {
	current, _, err := e.images.read()
	if err != nil {
		return "", err
	}
	if current != "" {
		return current, nil
	}
	if e.runningImage == "" {
		return "", fmt.Errorf("the agent's unit passed in no %s", agentImageEnv)
	}
	return e.runningImage, nil
}

func (e *containerAgentUpgradeExecutor) rollbackAfterStage(ctx context.Context, stageErr error) error {
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if rollbackErr := e.rollback(cleanupCtx); rollbackErr != nil {
		return fmt.Errorf("%w; rollback failed: %v", stageErr, rollbackErr)
	}
	return stageErr
}

func (e *containerAgentUpgradeExecutor) Abort(ctx context.Context) error {
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	if err := e.rollback(cleanupCtx); err != nil {
		// Preserve the signal so startup recovery can retry the rollback.
		return err
	}
	return e.signals.clear()
}

func (e *containerAgentUpgradeExecutor) rollback(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	signal, err := e.signals.read()
	if err != nil {
		return err
	}
	return e.rollbackSwitch(signal)
}

func (e *containerAgentUpgradeExecutor) Restart(ctx context.Context) error {
	return scheduleAgentRestart(ctx, e.runSystemdRun, time.Second)
}

func (e *containerAgentUpgradeExecutor) signalStore() agentUpgradeSignalStore { return e.signals }

func (e *containerAgentUpgradeExecutor) daemonInstance() string { return e.instanceID }

func (e *containerAgentUpgradeExecutor) validateStarted(signal *agentUpgradeSignal) error {
	if signal.CandidateImage == "" {
		return fmt.Errorf("AgentUpgrade was interrupted before selecting a candidate image")
	}
	current, _, err := e.images.read()
	if err != nil {
		return err
	}
	if current != signal.CandidateImage {
		return fmt.Errorf("AgentUpgrade was interrupted before switching to the candidate image")
	}
	if e.runningImage != signal.CandidateImage {
		return fmt.Errorf("the agent restarted on %q rather than the candidate image", e.runningImage)
	}
	if !validNspawnMachine(signal.ActiveMachine) {
		return fmt.Errorf("AgentUpgrade has no valid active nspawn machine")
	}
	running, err := e.runningExecutable()
	if err != nil {
		return err
	}
	// convergeContainerAgent copies the running agent into the active machine at startup.
	equal, err := filesHaveEqualSHA256(running, e.nspawnBinaryPath(signal.ActiveMachine))
	if err != nil {
		return fmt.Errorf("verify synchronized nspawn agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded agent and nspawn agent binaries do not match")
	}
	return nil
}

func (e *containerAgentUpgradeExecutor) candidateActive(signal *agentUpgradeSignal) (bool, error) {
	if signal == nil || signal.CandidateImage == "" {
		return false, nil
	}
	current, _, err := e.images.read()
	if err != nil {
		return false, err
	}
	return current == signal.CandidateImage, nil
}

func (e *containerAgentUpgradeExecutor) rollbackSwitch(signal *agentUpgradeSignal) error {
	if signal == nil {
		return nil
	}
	active, err := e.candidateActive(signal)
	if err != nil {
		return err
	}
	if !signal.SwitchCommitted && !signal.RecoveryRequired && !active {
		return nil
	}
	if signal.PreviousImage == "" {
		return fmt.Errorf("AgentUpgrade signal has no previous image to restore")
	}
	return e.images.write(signal.PreviousImage, signal.PreviousImage)
}

func (e *containerAgentUpgradeExecutor) runningLastGood(signal *agentUpgradeSignal) (bool, error) {
	// Without a previous image, the upgrade never switched away from the running one.
	return signal.PreviousImage == "" || e.runningImage == signal.PreviousImage, nil
}

func (e *containerAgentUpgradeExecutor) finish(
	ctx context.Context,
	c client.Client,
	operation agentdaemon.MachineOperation,
	result agentdaemon.MachineOperationResult[int64],
) error {
	return e.finishMachineOperation(ctx, c, operation, result)
}

// recoverContainerAgentUpgrade is the recovery unit's work in the agent's container, which runs
// it from the last-good image once the upgraded agent's unit has hit its start limit. As main's
// recovery script does, it records the failure, restores the last-good image, and restarts the
// agent's unit.
func recoverContainerAgentUpgrade(
	ctx context.Context,
	log *slog.Logger,
	message string,
	signals agentUpgradeSignalStore,
	images agentImageSelection,
	systemctl func(context.Context, ...string) error,
	runSystemdRun func(context.Context, ...string) error,
) error {
	if err := signals.recordFailure(message); err != nil {
		return err
	}
	signal, err := signals.read()
	if err != nil {
		return err
	}
	if signal == nil {
		// An ordinary failure of the agent's unit changes nothing.
		return nil
	}
	executor := &containerAgentUpgradeExecutor{signals: signals, images: images}
	if err := executor.rollbackSwitch(signal); err != nil {
		return err
	}
	// As kernel_t, which the agent's containers run as where containerd's files are unlabeled, as
	// ACL 3.0.20261019 ships them, reset-failed is denied. Then only a unit that systemd won't
	// restart, having hit its start limit, needs a restart, once the limit has passed: one still
	// restarting picks up the restored image on its own. On systemd 254 and later, OnFailure=
	// runs recovery on every failure, not only at the limit.
	if err := systemctl(ctx, "reset-failed", ServiceUnitName); err != nil {
		if failedErr := systemctl(ctx, "is-failed", "--quiet", ServiceUnitName); failedErr != nil {
			log.Info("the agent's unit is restarting on its own, onto the last-good image", "error", err)
			return nil
		}
		log.Warn("failed to reset the agent's unit; restarting it once its start limit has passed", "error", err)
		return scheduleAgentRestart(ctx, runSystemdRun, agentUnitStartLimitInterval)
	}
	return systemctl(ctx, "--no-block", "restart", ServiceUnitName)
}

// containerAgentConvergence keeps the host in step with the image the agent runs in, as the
// daemon on a host keeps its binary layout and units converged at every start: the agent's unit
// and its recovery unit, which a newer image can change, and the agent's binary in the active
// machine, which kubelet runs as its credential plugin and which an upgrade or rollback changes.
// It also removes the agent's images that it won't run again.
type containerAgentConvergence struct {
	log               *slog.Logger
	state             agentUpgradeStateLoader
	unitDir           string
	reload            func(context.Context) error
	runningExecutable func() (string, error)
	nspawnBinaryPath  func(string) string
	// For removing images: the agent's selection, the install's settings, the image this daemon
	// runs, and containerd.
	images       agentImageSelection
	install      agentImageSelection
	runningImage string
	listImages   func(context.Context) (string, error)
	ctr          func(context.Context, ...string) error
}

func newContainerAgentConvergence(log *slog.Logger, state agentUpgradeStateLoader) *containerAgentConvergence {
	return &containerAgentConvergence{
		log:     log,
		state:   state,
		unitDir: systemdSystemDir,
		reload: func(ctx context.Context) error {
			return utilexec.ReloadSystemd(ctx, log)
		},
		runningExecutable: runningAgentExecutable,
		nspawnBinaryPath:  activeNspawnAgentBinaryPath,
		images:            agentImageSelection{path: agentImageSelectionPath},
		install:           agentImageSelection{path: agentInstallSettingsPath},
		runningImage:      os.Getenv(agentImageEnv),
		listImages: func(ctx context.Context) (string, error) {
			return utilexec.OutputCmd(ctx, log, "ctr", "--namespace", agentContainerdNamespace, "images", "ls", "-q")
		},
		ctr: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Ctr(), args...)
		},
	}
}

func (c *containerAgentConvergence) converge(ctx context.Context) error {
	changed, err := writeContainerUnits(c.unitDir)
	if err != nil {
		return err
	}
	if changed {
		if err := c.reload(ctx); err != nil {
			return fmt.Errorf("systemctl daemon-reload: %w", err)
		}
		c.log.Info("updated the agent's units from its image")
	}
	c.removeImages(ctx)

	state, err := c.state.LoadState(ctx)
	if err != nil {
		return fmt.Errorf("load daemon state: %w", err)
	}
	if state == nil || !validNspawnMachine(state.ActiveMachine) {
		return nil
	}
	destination := c.nspawnBinaryPath(state.ActiveMachine)
	if _, err := os.Stat(filepath.Dir(destination)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	running, err := c.runningExecutable()
	if err != nil {
		return err
	}
	if equal, err := filesHaveEqualSHA256(running, destination); err == nil && equal {
		return nil
	}
	if err := copyExecutable(running, destination); err != nil {
		return fmt.Errorf("synchronize the agent binary in %s: %w", state.ActiveMachine, err)
	}
	c.log.Info("synchronized the agent binary in the active machine", "machine", state.ActiveMachine)
	return nil
}

// removeImages removes the agent's images it won't run again, as each AgentUpgrade pulls one: all
// but the selected and last good ones, the install's, which bootstrap runs after a reset, even
// offline, and the one this daemon runs. Failing to remove one is only logged, and failing to
// learn which to keep removes none.
func (c *containerAgentConvergence) removeImages(ctx context.Context) {
	current, lastGood, err := c.images.read()
	if err != nil {
		c.log.Warn("not removing old agent images", "error", err)
		return
	}
	install, _, err := c.install.read()
	if err != nil || install == "" {
		c.log.Warn("not removing old agent images: the install's image is unknown", "error", err)
		return
	}
	keep := map[string]bool{current: true, lastGood: true, install: true, c.runningImage: true}
	listed, err := c.listImages(ctx)
	if err != nil {
		c.log.Warn("not removing old agent images", "error", err)
		return
	}
	for _, image := range strings.Fields(listed) {
		if keep[image] {
			continue
		}
		if err := c.ctr(ctx, "--namespace", agentContainerdNamespace, "images", "rm", image); err != nil {
			c.log.Warn("failed to remove an old agent image", "image", image, "error", err)
			continue
		}
		c.log.Info("removed an old agent image", "image", image)
	}
}

// writeContainerUnits writes the agent's units in its container where they differ, and reports
// whether any did.
func writeContainerUnits(unitDir string) (bool, error) {
	changed := false
	for name, content := range map[string][]byte{
		ServiceUnitName:         containerServiceUnitContent,
		recoveryServiceUnitName: containerRecoveryUnitContent,
	} {
		path := filepath.Join(unitDir, name)
		current, err := os.ReadFile(path) //nolint:gosec // the agent's fixed unit paths
		if err == nil && bytes.Equal(current, content) {
			continue
		}
		if err := utilio.WriteFile(path, content, 0o644); err != nil {
			return changed, fmt.Errorf("write %s: %w", path, err)
		}
		changed = true
	}
	return changed, nil
}
