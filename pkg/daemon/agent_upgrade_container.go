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
	// agentContainerSlice holds the scopes systemd makes for the agent's containers, as their
	// units' ctr run asks runc to, so systemd tracks and accounts for them; ctr would otherwise
	// put them in cgroups of its own, outside any unit.
	agentContainerSlice = "system-aksflex.slice"
	// An AgentUpgrade's pull of its candidate, and its check that the candidate's agent runs, are
	// limited as a host's download and version check are, the check allowing for starting a
	// container.
	agentImagePullTimeout  = 10 * time.Minute
	agentImageCheckTimeout = time.Minute
	// agentImageCommandTimeout limits the agent's other calls to containerd about its images. The
	// daemon removes old images as it starts, before it reports an upgrade's result or reconciles,
	// and its unit counts as started already, so a call that hung would hold it there for good.
	agentImageCommandTimeout = time.Minute
	// agentUpgradeImageLabel marks the images an AgentUpgrade pulls, as it pulls them: the only
	// ones the agent removes once it won't run them again, and not an image staged, or built into
	// the OS, for a join.
	agentUpgradeImageLabel = "aks-flex-node.agent-upgrade"
	// agentUpgradeCheckContainer is the container in which an AgentUpgrade runs its candidate's
	// agent. Upgrades run one at a time, so one name serves them all, and a check clears what an
	// earlier one left, as the units clear their containers.
	agentUpgradeCheckContainer = "aks-flex-node-upgrade-check"
	// agentInstallSettingsPath is the host's provisioning's settings, which name the install's
	// image as AKS_FLEX_NODE_IMAGE. The entrypoint mounts them read-only.
	agentInstallSettingsPath = "/etc/sysconfig/aks-flex-node-container"
	// agentUnitStartLimitInterval is the StartLimitIntervalSec of the agent's unit in its container.
	agentUnitStartLimitInterval = 5 * time.Minute
)

// agentImageEnvFile is a systemd environment file that names images: the agent's selection, its
// image and last good one, or the install's settings, its image.
type agentImageEnvFile struct {
	path string
}

func (s agentImageEnvFile) read() (current, lastGood string, err error) {
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
func (s agentImageEnvFile) write(current, lastGood string) error {
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
	images  agentImageEnvFile
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
		images:       agentImageEnvFile{path: agentImageSelectionPath},
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
	pullCtx, cancelPull := context.WithTimeout(ctx, agentImagePullTimeout)
	defer cancelPull()
	pull := append([]string{"--namespace", agentContainerdNamespace, "image", "pull",
		"--label", agentUpgradeImageLabel + "=pulled"}, e.pullArgs...)
	if err := e.ctr(pullCtx, append(pull, request.image)...); err != nil {
		return fmt.Errorf("pull %s: %w", request.image, err)
	}
	if err := e.checkCandidate(ctx, request.image); err != nil {
		return err
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

// agentScopeAnnotations has runc set up a container's scope as the agent's units set up theirs,
// for unit, a unit name or %n in a unit: bound to the unit, so stopping it stops the container;
// killed 30 s into a stop, within the units' stop timeouts, so their stops end only once their
// containers have; and unloaded even when it fails, as a failed scope keeps the next container
// from its name, and on ACL nothing may reset it.
func agentScopeAnnotations(unit string) []string {
	return []string{
		"--annotation", fmt.Sprintf("org.systemd.property.BindsTo=['%s']", unit),
		"--annotation", "org.systemd.property.TimeoutStopUSec=uint64 30000000",
		"--annotation", "org.systemd.property.CollectMode='inactive-or-failed'",
	}
}

// checkCandidate runs the candidate's agent, as a host runs its candidate binary before switching
// to it. ctr run --rm removes the container after it, but not when ctr is killed, as a timeout, a
// cancellation, or the daemon's own end kills it, so the check clears the container before it
// runs, and after it fails. Its scope is bound to the agent's unit, so stopping the agent's unit
// stops the check too.
func (e *containerAgentUpgradeExecutor) checkCandidate(ctx context.Context, image string) error {
	e.removeCheckContainer(ctx)
	checkCtx, cancel := context.WithTimeout(ctx, agentImageCheckTimeout)
	defer cancel()
	check := append([]string{"--namespace", agentContainerdNamespace, "run", "--rm",
		"--runc-systemd-cgroup", "--cgroup", agentContainerSlice + ":aks-flex:upgrade-check"},
		agentScopeAnnotations(ServiceUnitName)...)
	check = append(check, e.runArgs...)
	check = append(check, image, agentUpgradeCheckContainer, "aks-flex-node", "version")
	if err := e.ctr(checkCtx, check...); err != nil {
		e.removeCheckContainer(ctx)
		return fmt.Errorf("run the agent in %s: %w", image, err)
	}
	return nil
}

// removeCheckContainer removes the check's task and container, if there are any, on a context of
// its own, which outlives a cancelled upgrade.
func (e *containerAgentUpgradeExecutor) removeCheckContainer(ctx context.Context) {
	cleanupCtx, cancel := agentUpgradeCleanupContext(ctx)
	defer cancel()
	_ = e.ctr(cleanupCtx, "--namespace", agentContainerdNamespace, "task", "delete", "--force", agentUpgradeCheckContainer)
	_ = e.ctr(cleanupCtx, "--namespace", agentContainerdNamespace, "container", "delete", agentUpgradeCheckContainer)
}

// currentImage is the image the agent runs, which its unit passed in, and which an upgrade rolls
// back to. Its unit runs the selected image, so the two agree, unless something selected another
// image since the agent started: the rollback would then restore an image that never ran here, so
// the upgrade fails instead.
func (e *containerAgentUpgradeExecutor) currentImage() (string, error) {
	if e.runningImage == "" {
		return "", fmt.Errorf("the agent's unit passed in no %s", agentImageEnv)
	}
	selected, _, err := e.images.read()
	if err != nil {
		return "", err
	}
	if selected != "" && selected != e.runningImage {
		return "", fmt.Errorf("%s selects %s, but the agent runs %s: restart the agent to run the selected image",
			e.images.path, selected, e.runningImage)
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
	return candidateImageActive(e.images, signal)
}

// candidateImageActive reports whether the agent's selection names the upgrade's candidate.
func candidateImageActive(images agentImageEnvFile, signal *agentUpgradeSignal) (bool, error) {
	if signal == nil || signal.CandidateImage == "" {
		return false, nil
	}
	current, _, err := images.read()
	if err != nil {
		return false, err
	}
	return current == signal.CandidateImage, nil
}

func (e *containerAgentUpgradeExecutor) rollbackSwitch(signal *agentUpgradeSignal) error {
	return rollbackImageSwitch(e.images, signal)
}

// rollbackImageSwitch selects the upgrade's previous image again, if the upgrade switched to its
// candidate, or may have.
func rollbackImageSwitch(images agentImageEnvFile, signal *agentUpgradeSignal) error {
	if signal == nil {
		return nil
	}
	active, err := candidateImageActive(images, signal)
	if err != nil {
		return err
	}
	if !signal.SwitchCommitted && !signal.RecoveryRequired && !active {
		return nil
	}
	if signal.PreviousImage == "" {
		return fmt.Errorf("AgentUpgrade signal has no previous image to restore")
	}
	return images.write(signal.PreviousImage, signal.PreviousImage)
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
	images agentImageEnvFile,
	systemctl func(context.Context, ...string) error,
	unitActiveState func(context.Context) (string, error),
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
	if err := rollbackImageSwitch(images, signal); err != nil {
		return err
	}
	// A host's security policy may deny the agent's container reset-failed on its unit, as an
	// immutable OS can: ACL does where its containers run as kernel_t, as on builds that leave
	// containerd's files unlabeled. Then only a unit that systemd won't restart, having hit its
	// start limit, needs starting, once the limit has passed: one still restarting picks up the
	// restored image on its own. On systemd 254 and later, OnFailure= runs recovery at every
	// failure, not only at the limit.
	if err := systemctl(ctx, "reset-failed", ServiceUnitName); err != nil {
		state, stateErr := unitActiveState(ctx)
		if stateErr == nil && state != "failed" {
			log.Info("the agent's unit is restarting on its own, onto the last-good image",
				"state", state, "error", err)
			return nil
		}
		log.Warn("failed to reset the agent's unit; starting it once its start limit has passed",
			"error", err, "state", state, "stateError", stateErr)
		return scheduleAgentStart(ctx, runSystemdRun, agentUnitStartLimitInterval)
	}
	return systemctl(ctx, "--no-block", "restart", ServiceUnitName)
}

// scheduleAgentStart starts the agent's unit from a transient timer, delay from now, for recovery
// that couldn't reset the unit. A start, not a restart: by then the unit may be running again, on
// its own or started by someone, and on the restored image, as the unit reads the selection at
// every start, and its start waits for recovery's. A start leaves a running unit alone, joins a
// restart in progress, and starts a failed one, so it's safe when recovery couldn't tell the
// unit's state, and when more than one recovery scheduled it.
func scheduleAgentStart(ctx context.Context, runSystemdRun func(context.Context, ...string) error, delay time.Duration) error {
	return scheduleAgentUnitCommand(ctx, runSystemdRun, delay, "aks-flex-node-agent-recovery-start", "start")
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
	images       agentImageEnvFile
	install      agentImageEnvFile
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
		images:            agentImageEnvFile{path: agentImageSelectionPath},
		install:           agentImageEnvFile{path: agentInstallSettingsPath},
		runningImage:      os.Getenv(agentImageEnv),
		listImages: func(ctx context.Context) (string, error) {
			return utilexec.OutputCmd(ctx, log, "ctr", "--namespace", agentContainerdNamespace, "images", "ls", "-q",
				fmt.Sprintf("labels.%q==pulled", agentUpgradeImageLabel))
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

// removeImages removes the images AgentUpgrades pulled, which carry agentUpgradeImageLabel, that
// the agent won't run again: all but the selected and last good ones, the install's, which
// bootstrap runs after a reset, even offline, and the one this daemon runs. Images it didn't pull
// for an upgrade, such as one staged for a join, stay. Failing to remove one is only logged, and
// failing to learn which to keep removes none.
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
	listCtx, cancelList := context.WithTimeout(ctx, agentImageCommandTimeout)
	listed, err := c.listImages(listCtx)
	cancelList()
	if err != nil {
		c.log.Warn("not removing old agent images", "error", err)
		return
	}
	for _, image := range strings.Fields(listed) {
		if keep[image] {
			continue
		}
		removeCtx, cancelRemove := context.WithTimeout(ctx, agentImageCommandTimeout)
		err := c.ctr(removeCtx, "--namespace", agentContainerdNamespace, "images", "rm", image)
		cancelRemove()
		if err != nil {
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
	// The recovery unit first, as on a host, so an interrupted write never leaves the agent's unit
	// pointing at a recovery unit that isn't there.
	for _, unit := range []struct {
		name    string
		content []byte
	}{
		{recoveryServiceUnitName, containerRecoveryUnitContent},
		{ServiceUnitName, containerServiceUnitContent},
	} {
		name, content := unit.name, unit.content
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
