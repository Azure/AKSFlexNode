package kube_binaries

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilhost"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

// Installer handles Kube binaries installation operations
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new Kube binaries Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute downloads and installs Kube binaries (kubelet, kubectl, kubeadm)
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Infof("Installing Kube Binaries of version %s", i.config.GetKubernetesVersion())

	// Download and install Kubernetes binaries
	if err := i.installKubeBinaries(ctx); err != nil {
		return fmt.Errorf("failed to install Kubernetes: %w", err)
	}

	i.logger.Info("Kubernetes binaries installed successfully")
	return nil
}

func (i *Installer) installKubeBinaries(ctx context.Context) error {
	// Clean up any corrupted installations before proceeding
	i.logger.Info("Cleaning up corrupted Kubernetes installation files to start fresh")
	if err := i.cleanupExistingInstallation(); err != nil {
		i.logger.Warnf("Failed to cleanup existing Kubernetes installation: %v", err)
		// Continue anyway - we'll install fresh
	}

	// Construct download URL
	_, url, err := i.constructKubeBinariesDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct Kubernetes download URL: %w", err)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, url) {
		if err != nil {
			return err
		}

		if !strings.HasPrefix(tarFile.Name, kubernetesTarPath) {
			continue
		}

		fileName := strings.TrimPrefix(tarFile.Name, kubernetesTarPath)
		targetFilePath := filepath.Join(binDir, fileName)

		i.logger.Debugf("extracting file %q to %q", tarFile.Name, targetFilePath)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0755); err != nil {
			return fmt.Errorf("failed to write file %q: %w", targetFilePath, err)
		}
	}

	return nil
}

// IsCompleted checks if all Kube binaries are installed
func (i *Installer) IsCompleted(ctx context.Context) bool {
	if i.canSkipKubernetesInstallation() {
		i.logger.Info("Kube binaries are already installed and valid, skipping installation")
		return true
	}
	return false
}

// Validate validates prerequisites for Kube binaries installation
func (i *Installer) Validate(ctx context.Context) error {
	// Verify network connectivity for download (basic check)
	kubernetesVersion := i.config.GetKubernetesVersion()
	if kubernetesVersion == "" {
		return fmt.Errorf("kubernetes version not specified")
	}
	return nil
}

// canSkipKubernetesInstallation checks if all Kube binaries are installed with the correct version
func (i *Installer) canSkipKubernetesInstallation() bool {
	for _, binaryPath := range kubeBinariesPaths {
		if !utils.FileExists(binaryPath) {
			i.logger.Debugf("Kubernetes binary not found: %s", binaryPath)
			return false
		}

		// Check version for kubelet (main component)
		if binaryPath == kubeletPath {
			if !i.isKubeletVersionCorrect() {
				i.logger.Debugf("Kubelet version is incorrect")
				return false
			}
		}
	}
	return true
}

// isKubeletVersionCorrect checks if the installed kubelet version matches the expected version
func (i *Installer) isKubeletVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(kubeletPath, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get kubelet version: %v", err)
		return false
	}

	// Check if version output contains expected version
	return strings.Contains(string(output), i.config.GetKubernetesVersion())
}

// cleanupExistingInstallation removes any existing Kubernetes installation that may be corrupted
func (i *Installer) cleanupExistingInstallation() error {
	i.logger.Debug("Cleaning up existing Kubernetes installation files")

	// Try to stop kubelet daemon process (best effort) - only kubelet runs as a daemon
	if err := utils.RunSystemCommand("pkill", "-f", "kubelet"); err != nil {
		i.logger.Debugf("No kubelet processes found to kill (or pkill failed): %v", err)
	}

	// List of binaries to clean up
	for _, binaryPath := range kubeBinariesPaths {
		if utils.FileExists(binaryPath) {
			i.logger.Debugf("Removing existing Kubernetes binary: %s", binaryPath)
			if err := utils.RunCleanupCommand(binaryPath); err != nil {
				i.logger.Warnf("Failed to remove %s: %v", binaryPath, err)
			}
		}
	}

	i.logger.Debug("Successfully cleaned up stale Kubernetes installation")
	return nil
}

// constructKubeBinariesDownloadURL constructs the download URL for the specified Kubernetes version
// it returns the file name and URL for downloading Kube binaries
func (i *Installer) constructKubeBinariesDownloadURL() (string, string, error) {
	arch := utilhost.GetArch()
	kubernetesVersion := i.config.GetKubernetesVersion()
	urlTemplate := i.getKubernetesURLTemplate()
	url := fmt.Sprintf(urlTemplate, kubernetesVersion, arch)
	fileName := fmt.Sprintf(kubernetesFileName, arch)
	i.logger.Infof("Constructed Kubernetes download URL: %s", url)
	return fileName, url, nil
}

func (i *Installer) getKubernetesURLTemplate() string {
	if i.config.Kubernetes.URLTemplate != "" {
		return i.config.Kubernetes.URLTemplate
	}
	// Default URL template for Kubernetes binaries
	return defaultKubernetesURLTemplate
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "KubeBinariesInstaller"
}
