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
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	ServiceUnitName         = "aks-flex-node-agent.service"
	recoveryServiceUnitName = "aks-flex-node-agent-recovery.service"
	recoveryScriptPath      = "/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"
	systemdSystemDir        = "/etc/systemd/system"
	arcSystemdDependency    = "himdsd.service"
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

func ensureAgentUpgradeServiceAssets(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	return ensureAgentUpgradeServiceAssetsAt(
		ctx,
		log,
		defaultAgentUpgradePaths(),
		agentServiceOptionsFromConfig(cfg),
		systemdSystemDir,
		recoveryScriptPath,
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
	recoveryContent := recoveryScriptContent
	for oldPath, newPath := range map[string]string{
		defaultAgentUpgradePaths().LastGoodPath: binaryPaths.LastGoodPath,
		defaultAgentUpgradePaths().SignalPath:   binaryPaths.SignalPath,
	} {
		recoveryContent = bytes.ReplaceAll(recoveryContent, []byte(oldPath), []byte(newPath))
	}
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

func writeAgentServiceAssets(binaryPaths agentUpgradePaths, serviceOptions agentServiceOptions, systemdDir, recoveryScript, currentBinaryPath string) error {
	assets, err := desiredAgentServiceAssets(binaryPaths, serviceOptions, systemdDir, recoveryScript, currentBinaryPath)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		if err := utilio.WriteFile(asset.path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write %s: %w", asset.path, err)
		}
		// Atomic replacement preserves an existing file's mode and applies the
		// process umask to new files, so reconcile the declared service-asset mode.
		if err := os.Chmod(asset.path, asset.mode); err != nil {
			return fmt.Errorf("set permissions on %s: %w", asset.path, err)
		}
	}
	return nil
}

type uninstallServiceTask struct {
	log *slog.Logger
}

// UninstallService returns a task that stops, disables, removes, and reloads the systemd unit.
func UninstallService(log *slog.Logger) phases.Task {
	return &uninstallServiceTask{log: log}
}

func (t *uninstallServiceTask) Name() string { return "uninstall-service" }

func (t *uninstallServiceTask) Do(ctx context.Context) error {
	if err := utilexec.StopService(ctx, t.log, ServiceUnitName); err != nil {
		t.log.Warn("failed to stop service (may not be running)", "unit", ServiceUnitName, "error", err)
	}
	if err := utilexec.DisableService(ctx, t.log, ServiceUnitName); err != nil {
		t.log.Warn("failed to disable service (may not be enabled)", "unit", ServiceUnitName, "error", err)
	}

	for _, path := range []string{
		filepath.Join(systemdSystemDir, ServiceUnitName),
		filepath.Join(systemdSystemDir, recoveryServiceUnitName),
		recoveryScriptPath,
		defaultAgentUpgradePaths().SignalPath,
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}

	if err := utilexec.ReloadSystemd(ctx, t.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	t.log.Info("systemd service uninstalled", "unit", ServiceUnitName)
	return nil
}
