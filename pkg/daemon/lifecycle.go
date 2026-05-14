package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const (
	ServiceUnitName  = "aks-flex-node-agent.service"
	systemdSystemDir = "/etc/systemd/system"
	azureConfigDir   = config.ConfigDir + "/azure"
)

var azureAuthFiles = []string{
	"azureProfile.json",
	"msal_token_cache.json",
	"msal_token_cache.bin",
	"clouds.config",
}

//go:embed assets/aks-flex-node-agent.service
var serviceUnitContent []byte

// InstallService installs the systemd unit and prepares root-owned Azure CLI auth files.
func InstallService(ctx context.Context, log *slog.Logger, azureConfigSourceDir string) error {
	unitPath := filepath.Join(systemdSystemDir, ServiceUnitName)
	if err := utilio.WriteFile(unitPath, serviceUnitContent, 0o644); err != nil { //nolint:gosec // service files must be world-readable
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if err := CopyAzureCLIAuth(azureConfigSourceDir); err != nil {
		return err
	}

	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	log.Info("systemd service installed", "unit", ServiceUnitName)
	return nil
}

type enableAndStartServiceTask struct {
	log *slog.Logger
}

func EnableAndStartServiceTask(log *slog.Logger) *enableAndStartServiceTask {
	return &enableAndStartServiceTask{log: log}
}

func (t *enableAndStartServiceTask) Name() string { return "enable-and-start-service" }

func (t *enableAndStartServiceTask) Do(ctx context.Context) error {
	if err := utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "enable", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", ServiceUnitName, err)
	}
	if err := utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "start", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl start %s: %w", ServiceUnitName, err)
	}

	t.log.Info("systemd service started", "unit", ServiceUnitName)
	return nil
}

// UninstallService stops, disables, removes, and reloads the systemd unit.
func UninstallService(ctx context.Context, log *slog.Logger) error {
	if err := utilexec.StopService(ctx, log, ServiceUnitName); err != nil {
		log.Warn("failed to stop service (may not be running)", "unit", ServiceUnitName, "error", err)
	}
	if err := utilexec.DisableService(ctx, log, ServiceUnitName); err != nil {
		log.Warn("failed to disable service (may not be enabled)", "unit", ServiceUnitName, "error", err)
	}

	unitPath := filepath.Join(systemdSystemDir, ServiceUnitName)
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", unitPath, err)
	}

	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	log.Info("systemd service uninstalled", "unit", ServiceUnitName)
	return nil
}

// CopyAzureCLIAuth copies only Azure CLI auth files into a root-owned directory.
func CopyAzureCLIAuth(sourceDir string) error {
	if sourceDir == "" {
		sourceDir = defaultAzureConfigSourceDir()
	}

	if err := os.MkdirAll(azureConfigDir, 0o700); err != nil {
		return fmt.Errorf("create azure config dir %s: %w", azureConfigDir, err)
	}
	if err := os.Chown(azureConfigDir, 0, 0); err != nil {
		return fmt.Errorf("chown azure config dir %s: %w", azureConfigDir, err)
	}
	if err := os.Chmod(azureConfigDir, 0o700); err != nil { // #nosec G302 -- directory must be traversable by root and inaccessible to other users
		return fmt.Errorf("chmod azure config dir %s: %w", azureConfigDir, err)
	}

	if sourceDir == "" {
		return nil
	}

	for _, name := range azureAuthFiles {
		sourcePath := filepath.Join(sourceDir, name)
		data, err := os.ReadFile(filepath.Clean(sourcePath)) // #nosec G304 -- fixed auth filenames under a caller-selected Azure config dir
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read azure auth file %s: %w", sourcePath, err)
		}

		destPath := filepath.Join(azureConfigDir, name)
		if err := utilio.WriteFile(destPath, data, 0o600); err != nil {
			return fmt.Errorf("write azure auth file %s: %w", destPath, err)
		}
		if err := os.Chown(destPath, 0, 0); err != nil {
			return fmt.Errorf("chown azure auth file %s: %w", destPath, err)
		}
		if err := os.Chmod(destPath, 0o600); err != nil {
			return fmt.Errorf("chmod azure auth file %s: %w", destPath, err)
		}
	}

	return nil
}

func defaultAzureConfigSourceDir() string {
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		if u, err := user.Lookup(sudoUser); err == nil && u.HomeDir != "" {
			return filepath.Join(u.HomeDir, ".azure")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".azure")
	}
	return ""
}
