package containerd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles containerd uninstallation operations
type UnInstaller struct {
	logger *logrus.Logger
}

// NewUnInstaller creates a new containerd unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "ContainerdUninstaller"
}

// Execute removes containerd container runtime and cleans up configuration
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Uninstalling containerd")

	// Step 1: Stop containerd services
	if err := u.stopContainerdServices(); err != nil {
		u.logger.Warnf("Failed to stop containerd services: %v (continuing)", err)
	}

	// Step 2: Remove containerd binaries
	if err := u.removeContainerdBinaries(); err != nil {
		return fmt.Errorf("failed to remove containerd binaries: %w", err)
	}

	// Step 3: Remove systemd service files
	if err := u.removeSystemdServices(); err != nil {
		return fmt.Errorf("failed to remove systemd services: %w", err)
	}

	// Step 4: Clean up configuration and data files
	if err := u.cleanupContainerdFiles(); err != nil {
		return fmt.Errorf("failed to cleanup containerd files: %w", err)
	}

	u.logger.Info("Containerd uninstalled successfully")
	return nil
}

// IsCompleted checks if containerd has been completely removed
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Check if any containerd binaries still exist (check all versions)
	for _, binary := range getAllContainerdBinaries() {
		if utils.BinaryExists(binary) {
			return false
		}
	}

	// Check if config file still exists
	if utils.FileExists(containerdConfigFile) {
		return false
	}

	// Check if service file still exists
	if utils.FileExists(containerdServiceFile) {
		return false
	}

	return true
}

// stopContainerdServices stops and disables all containerd-related services
func (u *UnInstaller) stopContainerdServices() error {
	u.logger.Info("Stopping and disabling containerd service")

	// Stop the service
	if err := utils.StopService("containerd"); err != nil {
		u.logger.Warnf("Failed to stop containerd service: %v", err)
	}

	// Disable the service
	if err := utils.RunSystemCommand("systemctl", "disable", "containerd"); err != nil {
		u.logger.Warnf("Failed to disable containerd service: %v", err)
	}

	return nil
}

// removeContainerdBinaries removes all containerd binary files
func (u *UnInstaller) removeContainerdBinaries() error {
	u.logger.Info("Removing containerd binaries")

	var binaryPaths []string
	// Add system binary paths (include all versions for complete cleanup)
	for _, binary := range getAllContainerdBinaries() {
		binaryPaths = append(binaryPaths, filepath.Join(systemBinDir, binary))
	}

	if fileErrors := utils.RemoveFiles(binaryPaths, u.logger); len(fileErrors) > 0 {
		for _, err := range fileErrors {
			u.logger.Warnf("Binary removal error: %v", err)
		}
	}

	return nil
}

// removeSystemdServices removes containerd systemd service files
func (u *UnInstaller) removeSystemdServices() error {
	u.logger.Info("Removing containerd systemd service")

	serviceFiles := []string{
		containerdServiceFile,
	}

	if fileErrors := utils.RemoveFiles(serviceFiles, u.logger); len(fileErrors) > 0 {
		for _, err := range fileErrors {
			u.logger.Warnf("Service file removal error: %v", err)
		}
	}

	// Reload systemd
	if err := utils.ReloadSystemd(); err != nil {
		u.logger.Warnf("Failed to reload systemd: %v", err)
		return err
	}

	return nil
}

// cleanupContainerdFiles removes containerd configuration and data files
func (u *UnInstaller) cleanupContainerdFiles() error {
	u.logger.Info("Cleaning up containerd configuration and data files")

	containerdDirectories := []string{
		containerdDataDir,
		defaultContainerdConfigDir,
	}

	// Remove directories recursively
	if dirErrors := utils.RemoveDirectories(containerdDirectories, u.logger); len(dirErrors) > 0 {
		for _, err := range dirErrors {
			u.logger.Warnf("Directory removal error: %v", err)
		}
	}

	return nil
}
