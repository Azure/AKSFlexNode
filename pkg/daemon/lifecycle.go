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
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	ServiceUnitName         = "aks-flex-node-agent.service"
	recoveryServiceUnitName = "aks-flex-node-agent-recovery.service"
	// recoveryScriptPath is the path baked into the embedded recovery unit.
	// It is the substitution placeholder rather than the install location: the
	// actual path is resolved from the configured host prefix, because /usr is
	// read-only on some hosts. Keep it in sync with the embedded asset.
	recoveryScriptPath = "/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"
	// systemdSystemDir is not prefix-relative. Units must live where systemd
	// looks for them, and /etc is writable even when /usr is not.
	systemdSystemDir     = "/etc/systemd/system"
	arcSystemdDependency = "himdsd.service"
)

//go:embed assets/aks-flex-node-agent.service
var serviceUnitContent []byte

var serviceUnitTemplate = template.Must(template.New(ServiceUnitName).Parse(string(serviceUnitContent)))

//go:embed assets/aks-flex-node-agent-recovery.service
var recoveryServiceUnitContent []byte

//go:embed assets/aks-flex-node-recovery.sh
var recoveryScriptContent []byte

type installServiceTask struct {
	cfg *config.Config
	log *slog.Logger
}

// InstallService returns a task that installs, enables, and starts the systemd unit.
func InstallService(cfg *config.Config, log *slog.Logger) phases.Task {
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

// recoveryScriptPathForPrefix returns where the recovery script is installed
// for a host install prefix. The default prefix reproduces the historical
// location, so hosts that do not set one are unaffected.
func recoveryScriptPathForPrefix(prefix string) string {
	return filepath.Join(goalstates.ResolveHostPaths(prefix).Prefix, "lib", "aks-flex-node", "aks-flex-node-recovery.sh")
}

// installedRecoveryScriptPath resolves the recovery script location for callers
// that have no config in hand, using the prefix this agent was installed with.
func installedRecoveryScriptPath() string {
	return recoveryScriptPathForPrefix(hostPrefixFromInstalledConfig())
}

func ensureAgentUpgradeServiceAssets(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	return ensureAgentUpgradeServiceAssetsAt(
		ctx,
		log,
		agentUpgradePathsForPrefix(cfg.Agent.HostPrefix),
		agentServiceOptionsFromConfig(cfg),
		systemdSystemDir,
		recoveryScriptPathForPrefix(cfg.Agent.HostPrefix),
		utilexec.ReloadSystemd,
	)
}

func ensureAgentUpgradeServiceAssetsAt(
	ctx context.Context,
	log *slog.Logger,
	binaryPaths agentUpgradePaths,
	serviceOptions agentServiceOptions,
	systemdDir, recoveryScript string,
	reload func(context.Context, *slog.Logger) error,
) error {
	if err := ensureAgentUpgradeLayout(ctx, log, binaryPaths); err != nil {
		return fmt.Errorf("initialize agent binary layout: %w", err)
	}
	if err := writeAgentServiceAssets(binaryPaths, serviceOptions, systemdDir, recoveryScript, binaryPaths.CurrentPath); err != nil {
		return err
	}
	if err := reload(ctx, log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
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
	recoveryServiceContent := bytes.ReplaceAll(recoveryServiceUnitContent, []byte(recoveryScriptPath), []byte(recoveryScript))
	recoveryContent := renderRecoveryScript(binaryPaths)
	// Publish dependencies before the main unit that references OnFailure, so an
	// interrupted update never leaves systemd pointing at missing recovery assets.
	return []agentServiceAsset{
		{path: recoveryScript, content: recoveryContent, mode: 0o750},
		{path: filepath.Join(systemdDir, recoveryServiceUnitName), content: recoveryServiceContent, mode: 0o644},
		{path: filepath.Join(systemdDir, ServiceUnitName), content: serviceContent, mode: 0o644},
	}, nil
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

// renderRecoveryScript points the embedded recovery script at the binary layout
// for this host.
//
// The embedded script contains the paths for the default prefix as literals, so
// those literals are what gets replaced. Keying the replacement on the paths
// resolved from the installed config instead matches nothing on a host with a
// custom prefix, and leaves the script reading last-good from /usr/local, where
// there is no binary, so a failed upgrade cannot be rolled back.
func renderRecoveryScript(binaryPaths agentUpgradePaths) []byte {
	embedded := agentUpgradePathsForPrefix("")
	content := recoveryScriptContent

	for oldPath, newPath := range map[string]string{
		embedded.LastGoodPath: binaryPaths.LastGoodPath,
		embedded.SignalPath:   binaryPaths.SignalPath,
	} {
		content = bytes.ReplaceAll(content, []byte(oldPath), []byte(newPath))
	}

	return content
}

func writeAgentServiceAssets(binaryPaths agentUpgradePaths, serviceOptions agentServiceOptions, systemdDir, recoveryScript, currentBinaryPath string) error {
	assets, err := desiredAgentServiceAssets(binaryPaths, serviceOptions, systemdDir, recoveryScript, currentBinaryPath)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if err := utilio.WriteFile(asset.path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write %s: %w", asset.path, err)
		}
	}
	return nil
}

type uninstallServiceTask struct {
	log    *slog.Logger
	prefix string
}

// UninstallService returns a task that stops, disables, removes, and reloads
// the systemd unit.
//
// The prefix is passed in rather than read from the installed config, because
// the daemon's reset paths remove /etc/aks-flex-node before they uninstall the
// service. Reading it at that point returns the default and leaves a prefixed
// recovery script behind. The recovery script is removed under the given
// prefix and under the default, so a host that changed prefix is still cleaned.
func UninstallService(log *slog.Logger, prefix string) phases.Task {
	return &uninstallServiceTask{log: log, prefix: prefix}
}

// InstalledHostPrefix returns the host prefix from the installed config, or the
// empty string when there is none. Callers that are about to remove the config
// read this first.
func InstalledHostPrefix() string {
	return hostPrefixFromInstalledConfig()
}

// uninstallPaths returns the files UninstallService removes for a prefix.
func uninstallPaths(prefix string) []string {
	paths := []string{
		filepath.Join(systemdSystemDir, ServiceUnitName),
		filepath.Join(systemdSystemDir, recoveryServiceUnitName),
	}
	for _, candidate := range goalstates.MergeHostPrefixes(prefix) {
		paths = append(paths, recoveryScriptPathForPrefix(candidate))
	}

	return append(paths, agentUpgradePathsForPrefix(prefix).SignalPath)
}

// removeIfPresent removes a file and treats its absence as success.
//
// It checks first. On a read-only filesystem, such as /usr on Azure Container
// Linux, unlinking a path that does not exist returns EROFS rather than ENOENT,
// so the default-prefix sweep would otherwise fail reset on a file that was
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
	if err := utilexec.StopService(ctx, t.log, ServiceUnitName); err != nil {
		t.log.Warn("failed to stop service (may not be running)", "unit", ServiceUnitName, "error", err)
	}
	if err := utilexec.DisableService(ctx, t.log, ServiceUnitName); err != nil {
		t.log.Warn("failed to disable service (may not be enabled)", "unit", ServiceUnitName, "error", err)
	}

	for _, path := range uninstallPaths(t.prefix) {
		if err := removeIfPresent(path); err != nil {
			return err
		}
	}

	if err := utilexec.ReloadSystemd(ctx, t.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	t.log.Info("systemd service uninstalled", "unit", ServiceUnitName)
	return nil
}
