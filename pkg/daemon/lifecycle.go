package daemon

import (
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
	ServiceUnitName  = "aks-flex-node-agent.service"
	systemdSystemDir = "/etc/systemd/system"
)

//go:embed assets/aks-flex-node-agent.service
var serviceUnitContent []byte

type installServiceTask struct {
	log *slog.Logger
}

// InstallService returns a task that installs, enables, and starts the systemd unit.
func InstallService(log *slog.Logger) phases.Task {
	return &installServiceTask{log: log}
}

func (t *installServiceTask) Name() string { return "install-service" }

func (t *installServiceTask) Do(ctx context.Context) error {
	unitPath := filepath.Join(systemdSystemDir, ServiceUnitName)
	if err := utilio.WriteFile(unitPath, serviceUnitContent, 0o644); err != nil { //nolint:gosec // service files must be world-readable
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if err := utilexec.ReloadSystemd(ctx, t.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
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

	unitPath := filepath.Join(systemdSystemDir, ServiceUnitName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}

	if err := utilexec.ReloadSystemd(ctx, t.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	t.log.Info("systemd service uninstalled", "unit", ServiceUnitName)
	return nil
}
