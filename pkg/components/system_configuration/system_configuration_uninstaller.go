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

	// Remove sysctl configuration
	if err := su.cleanupSysctlConfig(); err != nil {
		su.logger.WithError(err).Warn("Failed to cleanup sysctl configuration")
	}

	// Cleanup resolv.conf configuration
	if err := su.cleanupResolvConf(); err != nil {
		su.logger.WithError(err).Warn("Failed to cleanup resolv.conf configuration")
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
	// Check if sysctl config exists
	if utils.FileExists(sysctlConfigPath) {
		return false
	}
	// Note: We don't check resolv.conf as it may have been restored to original state
	// rather than removed entirely
	return true
}

// cleanupSysctlConfig removes the sysctl configuration
func (su *UnInstaller) cleanupSysctlConfig() error {
	if utils.FileExists(sysctlConfigPath) {
		if err := utils.RunCleanupCommand(sysctlConfigPath); err != nil {
			return err
		}
		su.logger.Info("Removed sysctl configuration file")
	}
	return nil
}

// cleanupResolvConf restores original resolv.conf configuration
func (su *UnInstaller) cleanupResolvConf() error {
	// Check if resolv.conf is a symlink to systemd-resolved that we created
	if utils.FileExists(resolvConfPath) {
		// Get link target
		output, err := utils.RunCommandWithOutput("readlink", resolvConfPath)
		if err == nil && output == resolvConfSource {
			// This is the symlink we created, remove it
			if err := utils.RunCleanupCommand(resolvConfPath); err != nil {
				return err
			}
			su.logger.Info("Removed resolv.conf symlink to systemd-resolved")
		} else {
			su.logger.Debug("resolv.conf is not our symlink, leaving unchanged")
		}
	}
	return nil
}
