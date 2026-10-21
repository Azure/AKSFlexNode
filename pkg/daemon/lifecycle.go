package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	ServiceUnitName         = "aks-flex-node-agent.service"
	recoveryServiceUnitName = "aks-flex-node-agent-recovery.service"
	// embeddedRecoveryScriptPath is the path the embedded recovery unit names,
	// the one releases before the host root installed to. It is replaced with
	// the path under the resolved host root when the unit is written. Keep it
	// in sync with the embedded asset.
	embeddedRecoveryScriptPath = "/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"
	// systemdSystemDir is not under the host root. Units must live where
	// systemd looks for them, and /etc is writable even when /usr is not.
	systemdSystemDir     = "/etc/systemd/system"
	arcSystemdDependency = "himdsd.service"

	// ServiceUnitPath is where the agent unit is installed. The first-boot unit
	// only runs while it is absent.
	ServiceUnitPath = systemdSystemDir + "/" + ServiceUnitName
	// FirstBootUnitName is the oneshot unit that runs bootstrap.sh on first
	// boot on a host provisioned by Ignition, as scripts/aks-flex-node-bootstrap.bu
	// documents. Reset removes it; a host without it is left alone.
	FirstBootUnitName = "aks-flex-node-bootstrap.service"
)

//go:embed assets/aks-flex-node-agent.service
var serviceUnitContent []byte

var serviceUnitTemplate = template.Must(template.New(ServiceUnitName).Parse(string(serviceUnitContent)))

//go:embed assets/aks-flex-node-agent-recovery.service
var recoveryServiceUnitContent []byte

//go:embed assets/aks-flex-node-nspawn-pre-start@.service
var nspawnPreStartUnitContent []byte

//go:embed assets/aks-flex-node-nspawn-post-start@.service
var nspawnPostStartUnitContent []byte

var (
	nspawnPreStartUnitTemplate  = template.Must(template.New(config.NSpawnPreStartUnit).Parse(string(nspawnPreStartUnitContent)))
	nspawnPostStartUnitTemplate = template.Must(template.New(config.NSpawnPostStartUnit).Parse(string(nspawnPostStartUnitContent)))
)

// nspawnLifecycleHelperName is the lifecycle helper unbounded copies beside the agent's binary
// for the machines' hooks to run, when the agent supplies no commands for them.
var nspawnLifecycleHelperName = filepath.Base(goalstates.LegacyHostPaths().NSpawnLifecycleBinary)

//go:embed assets/aks-flex-node-recovery.sh
var recoveryScriptContent []byte

type installServiceTask struct {
	cfg *config.Config
	log *slog.Logger
}

// InstallService returns a task that installs, enables, and starts the systemd unit. In the
// agent's container, the unit runs the agent's image instead of a binary on the host.
func InstallService(cfg *config.Config, log *slog.Logger) phases.Task {
	return installService(cfg, log, Containerized())
}

func installService(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	if containerized {
		return newInstallContainerServiceTask(log)
	}
	return &installServiceTask{cfg: cfg, log: log}
}

func (t *installServiceTask) Name() string { return "install-service" }

func (t *installServiceTask) Do(ctx context.Context) error {
	if err := ensureAgentUpgradeServiceAssets(ctx, t.log, t.cfg); err != nil {
		return err
	}
	if err := utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "enable", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", ServiceUnitName, err)
	}
	if err := utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "start", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl start %s: %w", ServiceUnitName, err)
	}

	t.log.Info("systemd service installed and started", "unit", ServiceUnitName)
	return nil
}

func ensureAgentUpgradeServiceAssets(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	_, err := ensureServiceAssetsUnderHostRoot(ctx, log, cfg, false)

	return err
}

// ensureDaemonServiceAssets converges the service assets as the daemon starts, as
// ensureAgentUpgradeServiceAssets does, except while an AgentUpgrade's signal exists. Then the
// daemon may be the upgrade's candidate, not yet judged, or recovery is under way, and the
// assets on disk are the last-good version's: recovery, and the restart into the last-good
// binary, run through them, so a candidate's broken assets would break its own rollback. It
// keeps them, writing only missing ones, and reports that it held them back, for the daemon to
// converge them once the upgrade is judged.
func ensureDaemonServiceAssets(ctx context.Context, log *slog.Logger, cfg *config.Config) (bool, error) {
	return ensureServiceAssetsUnderHostRoot(ctx, log, cfg, true)
}

// ensureServiceAssetsUnderHostRoot resolves the host root once, so the binary layout, the
// units, and the recovery script they run are all written under the same root.
func ensureServiceAssetsUnderHostRoot(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	holdDuringUpgrade bool,
) (bool, error) {
	root := hostroot.Resolve()

	return ensureAgentUpgradeServiceAssetsAt(
		ctx,
		log,
		agentUpgradePathsUnder(root),
		agentServiceOptionsFromConfig(cfg),
		systemdSystemDir,
		recoveryScriptPathUnder(root),
		utilexec.ReloadSystemd,
		holdDuringUpgrade,
	)
}

func ensureAgentUpgradeServiceAssetsAt(
	ctx context.Context,
	log *slog.Logger,
	binaryPaths agentUpgradePaths,
	serviceOptions agentServiceOptions,
	systemdDir, recoveryScript string,
	reload func(context.Context, *slog.Logger) error,
	holdDuringUpgrade bool,
) (bool, error) {
	if err := ensureAgentUpgradeLayout(ctx, log, binaryPaths); err != nil {
		return false, fmt.Errorf("initialize agent binary layout: %w", err)
	}
	if err := ensureNSpawnLifecycleHelperLink(binaryPaths.BinaryPath); err != nil {
		return false, err
	}
	held := false
	if holdDuringUpgrade {
		pending, err := agentUpgradePending(binaryPaths.SignalPath)
		if err != nil {
			return false, err
		}
		held = pending
	}
	if err := writeAgentServiceAssets(binaryPaths, serviceOptions, systemdDir, recoveryScript, binaryPaths.CurrentPath, held); err != nil {
		return false, err
	}
	if err := reload(ctx, log); err != nil {
		return false, fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return held, nil
}

type agentServiceOptions struct {
	ARCEnabled bool
}

func agentServiceOptionsFromConfig(cfg *config.Config) agentServiceOptions {
	return agentServiceOptions{ARCEnabled: cfg.IsARCEnabled()}
}

type agentServiceAsset struct {
	path    string
	content []byte
	mode    os.FileMode
}

func desiredAgentServiceAssets(binaryPaths agentUpgradePaths, serviceOptions agentServiceOptions, systemdDir, recoveryScript, currentBinaryPath string) ([]agentServiceAsset, error) {
	serviceContent, err := renderAgentServiceUnit(currentBinaryPath, serviceOptions)
	if err != nil {
		return nil, err
	}
	recoveryServiceContent := bytes.ReplaceAll(recoveryServiceUnitContent, []byte(embeddedRecoveryScriptPath), []byte(recoveryScript))
	recoveryContent := renderRecoveryScript(binaryPaths)
	lifecycleUnits, err := nspawnLifecycleUnitAssets(systemdDir, binaryPaths.BinaryPath)
	if err != nil {
		return nil, err
	}
	// Publish dependencies before the main unit that references OnFailure, so an
	// interrupted update never leaves systemd pointing at missing recovery assets.
	// The machines' hooks start the nspawn lifecycle units, and the daemon
	// rewrites the hooks once it has written these.
	return append(lifecycleUnits,
		agentServiceAsset{path: recoveryScript, content: recoveryContent, mode: 0o750},
		agentServiceAsset{path: filepath.Join(systemdDir, recoveryServiceUnitName), content: recoveryServiceContent, mode: 0o644},
		agentServiceAsset{path: filepath.Join(systemdDir, ServiceUnitName), content: serviceContent, mode: 0o644},
	), nil
}

// nspawnLifecycleUnitAssets returns the template units the machines' hooks start, which run the
// agent's nspawn-lifecycle phases through binaryPath: the agent's binary under the host root,
// which start installs before anything else, and which the binary layout later makes a link to
// the current one, so they always run the current agent.
func nspawnLifecycleUnitAssets(systemdDir, binaryPath string) ([]agentServiceAsset, error) {
	preStart, err := renderNSpawnLifecycleUnit(nspawnPreStartUnitTemplate, binaryPath)
	if err != nil {
		return nil, err
	}
	postStart, err := renderNSpawnLifecycleUnit(nspawnPostStartUnitTemplate, binaryPath)
	if err != nil {
		return nil, err
	}
	return []agentServiceAsset{
		{path: filepath.Join(systemdDir, config.NSpawnPreStartUnit), content: preStart, mode: 0o644},
		{path: filepath.Join(systemdDir, config.NSpawnPostStartUnit), content: postStart, mode: 0o644},
	}, nil
}

type ensureNSpawnLifecycleUnitsTask struct {
	log *slog.Logger
	// systemdDir, binaryPath and reload are the host's; tests replace them.
	systemdDir string
	binaryPath func() string
	reload     func(context.Context, *slog.Logger) error
}

// EnsureNSpawnLifecycleUnits returns a task that writes the nspawn lifecycle units the machines'
// hooks start, those that are missing, so that StartNode can start a machine: start installs the
// agent's other units only once the node runs. The daemon converges them with those units, while
// an AgentUpgrade isn't pending, so this leaves the existing ones as they are. It links the
// lifecycle helper too, for hooks an earlier release's pre-start writes, as after a reset that
// kept that release's binaries; see ensureNSpawnLifecycleHelperLink.
func EnsureNSpawnLifecycleUnits(log *slog.Logger) phases.Task {
	return &ensureNSpawnLifecycleUnitsTask{
		log:        log,
		systemdDir: systemdSystemDir,
		binaryPath: func() string { return agentUpgradePathsUnder(hostroot.Resolve()).BinaryPath },
		reload:     utilexec.ReloadSystemd,
	}
}

func (t *ensureNSpawnLifecycleUnitsTask) Name() string { return "ensure-nspawn-lifecycle-units" }

func (t *ensureNSpawnLifecycleUnitsTask) Do(ctx context.Context) error {
	binaryPath := t.binaryPath()
	if err := ensureNSpawnLifecycleHelperLink(binaryPath); err != nil {
		return err
	}
	assets, err := nspawnLifecycleUnitAssets(t.systemdDir, binaryPath)
	if err != nil {
		return err
	}
	written := false
	for _, asset := range assets {
		if _, err := os.Stat(asset.path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s: %w", asset.path, err)
		}
		if err := utilio.WriteFile(asset.path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write %s: %w", asset.path, err)
		}
		written = true
	}
	if !written {
		return nil
	}
	if err := t.reload(ctx, t.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

func installedAgentServiceOptions(systemdDir string) (agentServiceOptions, error) {
	path := filepath.Join(systemdDir, ServiceUnitName)
	content, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- path is built from the agent-owned systemd directory and fixed unit name
	if errors.Is(err, os.ErrNotExist) {
		return agentServiceOptions{}, nil
	}
	if err != nil {
		return agentServiceOptions{}, fmt.Errorf("read installed agent service %s: %w", path, err)
	}
	return agentServiceOptions{ARCEnabled: bytes.Contains(content, []byte(arcSystemdDependency))}, nil
}

type agentServiceTemplateData struct {
	CurrentBinaryPath string
	Dependencies      []string
}

func renderAgentServiceUnit(currentBinaryPath string, serviceOptions agentServiceOptions) ([]byte, error) {
	dependencies := []string{}
	if serviceOptions.ARCEnabled {
		dependencies = append(dependencies, arcSystemdDependency)
	}

	var content bytes.Buffer
	if err := serviceUnitTemplate.Execute(&content, agentServiceTemplateData{
		CurrentBinaryPath: currentBinaryPath,
		Dependencies:      dependencies,
	}); err != nil {
		return nil, fmt.Errorf("render agent systemd service: %w", err)
	}
	return content.Bytes(), nil
}

// renderNSpawnLifecycleUnit renders a template unit that runs an nspawn-lifecycle phase through
// binaryPath.
func renderNSpawnLifecycleUnit(unit *template.Template, binaryPath string) ([]byte, error) {
	var content bytes.Buffer
	if err := unit.Execute(&content, struct{ BinaryPath string }{BinaryPath: binaryPath}); err != nil {
		return nil, fmt.Errorf("render %s: %w", unit.Name(), err)
	}
	return content.Bytes(), nil
}

// ensureNSpawnLifecycleHelperLink makes the lifecycle helper beside the agent's binary a link to
// it. This release's hooks start its nspawn lifecycle units rather than run the helper, but a
// release from before them, after a rollback or a downgrade, writes hooks that run it, and
// without it the machine never starts again. As a link to the agent's binary, which is the
// current one, it runs whichever agent is installed, so it never goes stale and nothing has to
// keep it up to date, and it can stay: unbounded no longer writes the path once the agent
// supplies the hooks' commands. A copy an earlier release installed is replaced; reset and
// uninstall remove the link with the rest of the agent.
func ensureNSpawnLifecycleHelperLink(binaryPath string) error {
	helper := filepath.Join(filepath.Dir(binaryPath), nspawnLifecycleHelperName)
	target := filepath.Base(binaryPath)
	if current, err := os.Readlink(helper); err == nil && current == target {
		return nil
	}
	if err := replaceSymlink(helper, target); err != nil {
		return fmt.Errorf("link the nspawn lifecycle helper %s to %s: %w", helper, target, err)
	}
	return nil
}

// renderRecoveryScript points the embedded recovery script at the binary layout
// for this host.
//
// The embedded script contains the paths under the legacy root as literals, so
// those literals are what gets replaced. Left alone on a host installed under
// the host root, the script reads last-good from /usr/local, where there is no
// binary, so a failed upgrade cannot be rolled back.
func renderRecoveryScript(binaryPaths agentUpgradePaths) []byte {
	embedded := agentUpgradePathsUnder(hostroot.LegacyPath)
	content := recoveryScriptContent

	for oldPath, newPath := range map[string]string{
		embedded.LastGoodPath: binaryPaths.LastGoodPath,
		embedded.SignalPath:   binaryPaths.SignalPath,
	} {
		content = bytes.ReplaceAll(content, []byte(oldPath), []byte(newPath))
	}

	return content
}

// agentUpgradePending reports whether an AgentUpgrade's signal exists: an upgrade awaits its
// judgment, or recovery is under way.
func agentUpgradePending(signalPath string) (bool, error) {
	if _, err := os.Stat(signalPath); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("check for a pending AgentUpgrade: %w", err)
	}
	return false, nil
}

// writeAgentServiceAssets writes the agent's service assets, but with keepExisting, only those
// that are missing.
func writeAgentServiceAssets(binaryPaths agentUpgradePaths, serviceOptions agentServiceOptions, systemdDir, recoveryScript, currentBinaryPath string, keepExisting bool) error {
	assets, err := desiredAgentServiceAssets(binaryPaths, serviceOptions, systemdDir, recoveryScript, currentBinaryPath)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if keepExisting {
			if _, err := os.Stat(asset.path); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("check %s: %w", asset.path, err)
			}
		}
		if err := utilio.WriteFile(asset.path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write %s: %w", asset.path, err)
		}
	}
	return nil
}

type uninstallServiceTask struct {
	log *slog.Logger
	// systemctl, unitDir and paths are the host's; tests replace them.
	systemctl func(ctx context.Context, args ...string) error
	unitDir   string
	paths     []string
	// flatten leaves the agent's binary without its blue/green layout; nil does nothing.
	flatten func() error
}

// UninstallService returns a task that removes the first-boot unit, then
// disables, removes, and stops the agent's systemd unit, leaving the agent's
// binary under the host root as a regular file, without its blue/green layout.
//
// The paths are resolved when the task is built. Nothing in reset changes the
// host root, and ResetNode resolves the helper paths it removes the same way.
func UninstallService(log *slog.Logger) phases.Task {
	binaryPaths := agentUpgradePathsUnder(hostroot.Resolve())
	return &uninstallServiceTask{
		log: log,
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
		unitDir: systemdSystemDir,
		paths:   uninstallPaths(),
		flatten: func() error { return flattenAgentLayout(log, binaryPaths, agentUpgradeLockPath) },
	}
}

// uninstallPaths returns the files UninstallService removes. The recovery
// script is removed under the resolved host root and under the legacy root, so
// that uninstalling does not depend on the host root having been migrated.
func uninstallPaths() []string {
	return uninstallPathsUnder(hostroot.Resolve(), hostroot.LegacyPath)
}

func uninstallPathsUnder(root, legacy string) []string {
	paths := []string{
		filepath.Join(systemdSystemDir, ServiceUnitName),
		filepath.Join(systemdSystemDir, recoveryServiceUnitName),
		filepath.Join(systemdSystemDir, config.NSpawnPreStartUnit),
		filepath.Join(systemdSystemDir, config.NSpawnPostStartUnit),
		recoveryScriptPathUnder(root),
	}
	if root != legacy {
		paths = append(paths, recoveryScriptPathUnder(legacy))
	}

	return append(paths, agentUpgradePathsUnder(root).SignalPath)
}

// removeIfPresent removes a file and treats its absence as success.
//
// It checks first. On a read-only filesystem, such as /usr on Azure Container
// Linux, unlinking a path that does not exist returns EROFS rather than ENOENT,
// so the sweep of the legacy root would otherwise fail reset on a file that was
// never there. Lstat so a dangling symlink still counts as present.
func removeIfPresent(path string) error {
	return removeIfPresentWith(path, os.Lstat, os.Remove)
}

func removeIfPresentWith(path string, lstat func(string) (os.FileInfo, error), remove func(string) error) error {
	if _, err := lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

func (t *uninstallServiceTask) Name() string { return "uninstall-service" }

func (t *uninstallServiceTask) Do(ctx context.Context) error {
	if err := removeFirstBootUnit(ctx, t.log, t.unitDir, t.systemctl); err != nil {
		return err
	}
	// Stopping comes last. When the daemon uninstalls its own unit, for AgentReset or a node's
	// deletion, stopping the unit stops the daemon, and nothing after the stop would run: the
	// unit would stay enabled, behind a dangling .wants link, with systemd not reloaded. systemd
	// still stops a running unit whose file is gone.
	if err := t.systemctl(ctx, "disable", ServiceUnitName); err != nil {
		t.log.Warn("failed to disable service (may not be enabled)", "unit", ServiceUnitName, "error", err)
	}
	for _, path := range t.paths {
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}
	if err := t.systemctl(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	t.log.Info("systemd service uninstalled", "unit", ServiceUnitName)

	// Once no unit names the layout, so nothing restarts the agent from it, and before the stop,
	// which ends a daemon resetting its own node. A layout left behind does no harm: start
	// replaces it with the binary that runs it.
	if t.flatten != nil {
		if err := t.flatten(); err != nil {
			t.log.Warn("kept the agent's blue/green binary layout; the next start replaces it", "error", err)
		}
	}

	if err := t.systemctl(ctx, "stop", ServiceUnitName); err != nil {
		t.log.Warn("failed to stop service (may not be running)", "unit", ServiceUnitName, "error", err)
	}
	return nil
}

// removeFirstBootUnit disables, stops, and removes the first-boot bootstrap
// unit, if the host was provisioned with one. On any other host the unit file
// is absent and nothing is done.
//
// --now matters. The unit is a oneshot with RemainAfterExit=yes, so without a
// stop it stays active after its file is gone, and starting it again after the
// host is provisioned anew does nothing.
func removeFirstBootUnit(
	ctx context.Context,
	log *slog.Logger,
	unitDir string,
	systemctl func(context.Context, ...string) error,
) error {
	unitPath := filepath.Join(unitDir, FirstBootUnitName)
	if _, err := os.Lstat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	log.Info("removing first-boot bootstrap unit", "unit", FirstBootUnitName)
	if err := systemctl(ctx, "disable", "--now", FirstBootUnitName); err != nil {
		return fmt.Errorf("systemctl disable --now %s: %w", FirstBootUnitName, err)
	}

	return removeIfPresent(unitPath)
}
