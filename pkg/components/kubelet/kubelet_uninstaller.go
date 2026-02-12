package kubelet

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles kubelet cleanup operations
type UnInstaller struct {
	logger *logrus.Logger
}

// NewUnInstaller creates a new kubelet unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		logger: logger,
	}
}

// GetName returns the step name for the executor interface
func (u *UnInstaller) GetName() string {
	return "KubeletUnInstaller"
}

// Execute removes kubelet configuration and runtime files
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Cleaning up kubelet configuration")

	// Stop kubelet service first if it's running
	if utils.ServiceExists("kubelet") {
		if err := utils.StopService("kubelet"); err != nil {
			u.logger.Warnf("Failed to stop kubelet service: %v (continuing)", err)
		}
	}

	// Remove kubelet configuration files
	kubeletFiles := []string{
		kubeletDefaultsPath,
		kubeletServicePath,
		kubeletContainerdConfig,
		kubeletConfigPath,
		kubeletKubeConfig,
		kubeletBootstrapKubeConfig,
		kubeletTokenScriptPath,
		apiserverClientCAPath,
	}

	// Remove kubelet configuration directories
	kubeletDirectories := []string{
		kubeletServiceDir,      // /etc/systemd/system/kubelet.service.d
		kubeletVarDir,          // /var/lib/kubelet
		kubeletManifestsDir,    // Static pod manifests (kubelet-specific)
		kubeletVolumePluginDir, // Volume plugins (kubelet-specific)
	}

	// Remove individual files
	if fileErrors := utils.RemoveFiles(kubeletFiles, u.logger); len(fileErrors) > 0 {
		for _, err := range fileErrors {
			u.logger.Warnf("Configuration file removal error: %v", err)
		}
	}

	// Remove directories
	if dirErrors := utils.RemoveDirectories(kubeletDirectories, u.logger); len(dirErrors) > 0 {
		for _, err := range dirErrors {
			u.logger.Warnf("Directory removal error: %v", err)
		}
	}

	// Reload systemd to clean up service definitions
	if err := utils.ReloadSystemd(); err != nil {
		u.logger.Warnf("Failed to reload systemd: %v", err)
	}

	u.logger.Info("Kubelet configuration cleanup completed")
	return nil
}

// IsCompleted checks if kubelet configuration files have been removed
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Check critical configuration files
	criticalFiles := []string{
		kubeletConfigPath,
		kubeletKubeConfig,
		kubeletBootstrapKubeConfig,
		kubeletTokenScriptPath, // Check token script cleanup
	}

	for _, file := range criticalFiles {
		if utils.FileExists(file) {
			return false
		}
	}

	return true
}
