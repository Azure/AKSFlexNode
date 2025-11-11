package system_configuration

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles system configuration cleanup
type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

// NewUnInstaller creates a new system configuration unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (su *UnInstaller) GetName() string {
	return "SystemCleanup"
}

// Execute removes system configuration files and resets settings
func (su *UnInstaller) Execute(ctx context.Context) error {
	su.logger.Info("Cleaning up system configuration")

	// Remove current sysctl configuration
	if err := su.cleanupSysctlConfig(); err != nil {
		su.logger.WithError(err).Warn("Failed to cleanup sysctl configuration")
	}

	// Remove stale configuration files
	if err := su.cleanupStaleFiles(); err != nil {
		su.logger.WithError(err).Warn("Failed to cleanup legacy configuration files")
	}

	// Reload sysctl to apply changes
	if err := utils.RunSystemCommand("sysctl", "--system"); err != nil {
		su.logger.WithError(err).Warn("Failed to reload sysctl settings")
	}

	su.logger.Info("System configuration cleanup completed")
	return nil
}

// IsCompleted checks if system configuration has been removed
func (su *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Check if current sysctl config exists
	if utils.FileExists(SysctlConfigPath) {
		return false
	}

	// Check if legacy config files exist
	legacyFiles := []string{
		LegacySysctlConfig,
		LegacyContainerdConf,
	}

	for _, file := range legacyFiles {
		if utils.FileExists(file) {
			return false
		}
	}

	return true
}

// cleanupSysctlConfig removes the current sysctl configuration
func (su *UnInstaller) cleanupSysctlConfig() error {
	if utils.FileExists(SysctlConfigPath) {
		if err := utils.RunCleanupCommand(SysctlConfigPath); err != nil {
			return err
		}
		su.logger.Info("Removed sysctl configuration file")
	}
	return nil
}

// cleanupStaleFiles removes stale configuration files
func (su *UnInstaller) cleanupStaleFiles() error {
	legacyFiles := []string{
		LegacySysctlConfig,
		LegacyContainerdConf,
	}

	for _, file := range legacyFiles {
		if utils.FileExists(file) {
			if err := utils.RunCleanupCommand(file); err != nil {
				su.logger.WithError(err).Warnf("Failed to remove file: %s", file)
				continue
			}
			su.logger.Infof("Removed legacy configuration file: %s", file)
		}
	}

	return nil
}
