package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	ServiceUnitName         = "aks-flex-node-agent.service"
	recoveryServiceUnitName = "aks-flex-node-agent-recovery.service"
	recoveryScriptPath      = "/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"
	systemdSystemDir        = "/etc/systemd/system"
)

//go:embed assets/aks-flex-node-agent.service
var serviceUnitContent []byte

//go:embed assets/aks-flex-node-agent-recovery.service
var recoveryServiceUnitContent []byte

//go:embed assets/aks-flex-node-recovery.sh
var recoveryScriptContent []byte

type installServiceTask struct {
	log *slog.Logger
}

// InstallService returns a task that installs, enables, and starts the systemd unit.
func InstallService(log *slog.Logger) phases.Task {
	return &installServiceTask{log: log}
}

func (t *installServiceTask) Name() string { return "install-service" }

func (t *installServiceTask) Do(ctx context.Context) error {
	if err := ensureAgentUpgradeServiceAssets(ctx, t.log); err != nil {
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

func ensureAgentUpgradeServiceAssets(ctx context.Context, log *slog.Logger) error {
	return ensureAgentUpgradeServiceAssetsAt(
		ctx,
		log,
		defaultAgentUpgradePaths(),
		systemdSystemDir,
		recoveryScriptPath,
		utilexec.ReloadSystemd,
	)
}

func ensureAgentUpgradeServiceAssetsAt(
	ctx context.Context,
	log *slog.Logger,
	binaryPaths agentUpgradePaths,
	systemdDir, recoveryScript string,
	reload func(context.Context, *slog.Logger) error,
) error {
	if err := ensureAgentUpgradeLayout(ctx, log, binaryPaths); err != nil {
		return fmt.Errorf("initialize agent binary layout: %w", err)
	}
	if err := writeAgentServiceAssets(binaryPaths, systemdDir, recoveryScript, binaryPaths.CurrentPath); err != nil {
		return err
	}
	if err := reload(ctx, log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

type agentServiceAsset struct {
	path    string
	content []byte
	mode    os.FileMode
}

func desiredAgentServiceAssets(binaryPaths agentUpgradePaths, systemdDir, recoveryScript, currentBinaryPath string) []agentServiceAsset {
	serviceContent := bytes.ReplaceAll(serviceUnitContent, []byte(defaultAgentUpgradePaths().BinaryPath), []byte(currentBinaryPath))
	recoveryServiceContent := bytes.ReplaceAll(recoveryServiceUnitContent, []byte(recoveryScriptPath), []byte(recoveryScript))
	recoveryContent := recoveryScriptContent
	for oldPath, newPath := range map[string]string{
		defaultAgentUpgradePaths().LastGoodPath: binaryPaths.LastGoodPath,
		defaultAgentUpgradePaths().SignalPath:   binaryPaths.SignalPath,
	} {
		recoveryContent = bytes.ReplaceAll(recoveryContent, []byte(oldPath), []byte(newPath))
	}
	return []agentServiceAsset{
		{path: filepath.Join(systemdDir, ServiceUnitName), content: serviceContent, mode: 0o644},
		{path: filepath.Join(systemdDir, recoveryServiceUnitName), content: recoveryServiceContent, mode: 0o644},
		{path: recoveryScript, content: recoveryContent, mode: 0o750},
	}
}

func writeAgentServiceAssets(binaryPaths agentUpgradePaths, systemdDir, recoveryScript, currentBinaryPath string) error {
	for _, asset := range desiredAgentServiceAssets(binaryPaths, systemdDir, recoveryScript, currentBinaryPath) {
		if err := utilio.WriteFile(asset.path, asset.content, asset.mode); err != nil {
			return fmt.Errorf("write %s: %w", asset.path, err)
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
