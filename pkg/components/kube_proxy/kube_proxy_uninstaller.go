package kube_proxy

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles kube-proxy uninstallation operations
type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

// NewUnInstaller creates a new kube-proxy UnInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the step name for the executor interface
func (u *UnInstaller) GetName() string {
	return "KubeProxyUnInstaller"
}

// Execute removes kube-proxy service and configuration
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Uninstalling kube-proxy")

	// Remove configuration files
	if err := u.removeConfigurationFiles(); err != nil {
		u.logger.Warnf("Failed to remove kube-proxy configuration files: %v", err)
		// Continue with cleanup
	}

	// Remove systemd service files
	if err := u.removeServiceFiles(); err != nil {
		u.logger.Warnf("Failed to remove kube-proxy service files: %v", err)
		// Continue with cleanup
	}

	// Remove directories if empty
	if err := u.cleanupDirectories(); err != nil {
		u.logger.Warnf("Failed to cleanup kube-proxy directories: %v", err)
		// Continue with cleanup
	}

	// Remove kube-proxy binary
	if err := u.removeBinary(); err != nil {
		u.logger.Warnf("Failed to remove kube-proxy binary: %v", err)
		// Continue with cleanup
	}

	u.logger.Info("Kube-proxy uninstalled successfully")
	return nil
}

// IsCompleted checks if kube-proxy has been completely uninstalled
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	u.logger.Debug("Checking kube-proxy uninstallation status")

	// Check if service is still running
	if utils.IsServiceActive(kubeProxyServiceName) {
		u.logger.Debug("kube-proxy service is still active")
		return false
	}

	// Check if service is still enabled
	if utils.IsServiceEnabled(kubeProxyServiceName) {
		u.logger.Debug("kube-proxy service is still enabled")
		return false
	}

	// Check if configuration files still exist
	configFiles := []string{
		kubeProxyConfigPath,
		kubeProxyKubeConfig,
		kubeProxyServicePath,
		kubeProxyDropInPath,
	}

	for _, file := range configFiles {
		if utils.FileExistsAndValid(file) {
			u.logger.Debugf("kube-proxy configuration file still exists: %s", file)
			return false
		}
	}

	// Check if binary still exists
	if utils.FileExistsAndValid(kubeProxyBinaryPath) {
		u.logger.Debug("kube-proxy binary still exists")
		return false
	}

	u.logger.Debug("Kube-proxy appears to be completely uninstalled")
	return true
}

// removeConfigurationFiles removes all kube-proxy configuration files
func (u *UnInstaller) removeConfigurationFiles() error {
	u.logger.Info("Removing kube-proxy configuration files")

	configFiles := []string{
		kubeProxyConfigPath,
		kubeProxyKubeConfig,
	}

	for _, file := range configFiles {
		if err := utils.RunCleanupCommand(file); err != nil {
			u.logger.Warnf("Failed to remove configuration file %s: %v", file, err)
		} else if utils.FileExistsAndValid(file) {
			u.logger.Debugf("Removed kube-proxy configuration file: %s", file)
		}
	}

	return nil
}

// removeServiceFiles removes systemd service files
func (u *UnInstaller) removeServiceFiles() error {
	u.logger.Info("Removing kube-proxy systemd service files")

	serviceFiles := []string{
		kubeProxyServicePath,
		kubeProxyDropInPath,
	}

	for _, file := range serviceFiles {
		if err := utils.RunCleanupCommand(file); err != nil {
			u.logger.Warnf("Failed to remove service file %s: %v", file, err)
		} else if utils.FileExistsAndValid(file) {
			u.logger.Debugf("Removed kube-proxy service file: %s", file)
		}
	}

	// Reload systemd daemon after removing service files
	if err := utils.RunSystemCommand("systemctl", "daemon-reload"); err != nil {
		u.logger.Warnf("Failed to reload systemd daemon: %v", err)
	}

	return nil
}

// cleanupDirectories removes empty kube-proxy directories
func (u *UnInstaller) cleanupDirectories() error {
	u.logger.Info("Cleaning up kube-proxy directories")

	// Remove directories if they're empty
	dirs := []string{
		kubeProxyServiceDropIn,
		kubeProxyVarDir,
	}

	for _, dir := range dirs {
		// Only remove if directory is empty
		if err := utils.RunSystemCommand("rmdir", dir); err != nil {
			u.logger.Debugf("Directory %s not empty or doesn't exist, skipping: %v", dir, err)
		} else {
			u.logger.Debugf("Removed empty directory: %s", dir)
		}
	}

	return nil
}

// removeBinary removes the kube-proxy binary that we downloaded
func (u *UnInstaller) removeBinary() error {
	u.logger.Info("Removing kube-proxy binary")

	if err := utils.RunCleanupCommand(kubeProxyBinaryPath); err != nil {
		u.logger.Warnf("Failed to remove kube-proxy binary %s: %v", kubeProxyBinaryPath, err)
	} else if utils.FileExistsAndValid(kubeProxyBinaryPath) {
		u.logger.Debugf("Removed kube-proxy binary: %s", kubeProxyBinaryPath)
	}

	return nil
}
