package cni

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles CNI setup and installation operations
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new CNI setup Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "CNISetup"
}

// Validate validates prerequisites for CNI setup
func (i *Installer) Validate(ctx context.Context) error {
	// Validate CNI version format
	cniVersion := getCNIVersion(i.config)
	if cniVersion == "" {
		return fmt.Errorf("CNI version cannot be empty")
	}
	return nil
}

// Execute configures the container network interface plugins and settings
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Setting up Container Network Interface (CNI) configuration")

	// Prepare CNI directories
	i.logger.Info("Step 1: Preparing CNI directories")
	if err := i.prepareCNIDirectories(); err != nil {
		return fmt.Errorf("failed to prepare CNI directories: %w", err)
	}
	i.logger.Info("CNI directories are ready")

	// Install CNI plugins
	i.logger.Info("Step 2: Installing CNI plugins")
	if err := i.installCNIPlugins(); err != nil {
		i.logger.Errorf("CNI plugins installation failed: %v", err)
		return fmt.Errorf("failed to install CNI plugins version %s: %w", DefaultCNIVersion, err)
	}
	i.logger.Info("CNI plugins installed successfully")

	// Create bridge configuration for edge node
	i.logger.Info("Step 3: Creating bridge configuration")
	if err := i.createBridgeConfig(); err != nil {
		i.logger.Errorf("Bridge configuration creation failed: %v", err)
		return fmt.Errorf("failed to create bridge config: %w", err)
	}
	i.logger.Info("Bridge configuration created successfully")

	i.logger.Info("CNI setup completed successfully")
	return nil
}

// IsCompleted checks if CNI configuration has been set up properly
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Validate Step 1: CNI directories preparation
	for _, dir := range cniDirs {
		if !utils.DirectoryExists(dir) {
			i.logger.Debugf("CNI directory not found: %s", dir)
			return false
		}
	}

	// Validate Step 2: CNI plugin binaries
	for _, plugin := range requiredCNIPlugins {
		pluginPath := filepath.Join(DefaultCNIBinDir, plugin)
		if !utils.FileExistsAndValid(pluginPath) {
			i.logger.Debugf("CNI plugin not found: %s", plugin)
			return false
		}
	}

	// Validate Step 3: Bridge configuration
	configPath := filepath.Join(DefaultCNIConfDir, bridgeConfigFile)
	if !utils.FileExistsAndValid(configPath) {
		i.logger.Debug("Bridge configuration file not found")
		return false
	}

	i.logger.Debug("CNI setup validation passed - all components properly configured")
	return true
}

func (i *Installer) prepareCNIDirectories() error {
	for _, dir := range cniDirs {
		if !utils.DirectoryExists(dir) {
			// Create directory if it doesn't exist
			if err := utils.RunSystemCommand("mkdir", "-p", dir); err != nil {
				return fmt.Errorf("failed to create CNI directory %s: %w", dir, err)
			}
		}

		// Only clean configuration, not binaries
		if dir == DefaultCNIConfDir {
			i.logger.Debugf("Cleaning existing CNI configurations in: %s", dir)
			if err := utils.RunSystemCommand("rm", "-rf", dir+"/*"); err != nil {
				return fmt.Errorf("failed to clean CNI configuration directory: %w", err)
			}
		}

		// Set proper permissions
		if err := utils.RunSystemCommand("chmod", "-R", "0755", dir); err != nil {
			logrus.Warnf("Failed to set permissions for CNI directory %s: %v", dir, err)
		}
	}
	return nil
}

// installCNIPlugins downloads and installs CNI plugins (matching reference script)
func (i *Installer) installCNIPlugins() error {
	if canSkipCNIPluginInstallation() {
		logrus.Info("CNI plugins are already installed and valid, skipping installation")
		return nil
	}

	// Clean up any corrupted installations before proceeding
	logrus.Info("Cleaning up corrupted CNI plugins files to start fresh")
	if err := utils.RunSystemCommand("rm", "-rf", DefaultCNIBinDir+"/*"); err != nil {
		logrus.Warnf("Failed to clean CNI bin directory: %v", err)
	}

	// Ensure curl is installed before using it for downloading CNI plugins
	if err := utils.RunSystemCommand("which", "curl"); err != nil {
		logrus.Info("Installing curl...")
		if err := utils.RunSystemCommand("apt", "install", "-y", "curl"); err != nil {
			logrus.Warnf("Failed to install curl: %v", err)
		}
	}

	// Construct CNI download URL
	cniFileName, cniDownloadURL, err := i.constructCNIDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct CNI download URL: %w", err)
	}

	// Download the CNI plugin tar file into tmp directory
	tempFile := fmt.Sprintf("/tmp/%s", cniFileName)
	// Clean up any existing CNI temp files from both /tmp and home directory
	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -f %s", tempFile)); err != nil {
		logrus.Warnf("Failed to clean up existing CNI temp files from /tmp: %s", err)
	}
	if err := utils.RunSystemCommand("curl", "-o", tempFile, "-L", cniDownloadURL); err != nil {
		return fmt.Errorf("failed to download CNI plugins: %w", err)
	}
	defer func() {
		if err := utils.RunCleanupCommand(tempFile); err != nil {
			logrus.Warnf("Failed to clean up temp file %s: %v", tempFile, err)
		}
	}()

	// Extract CNI plugins to /opt/cni/bin
	if err := utils.RunSystemCommand("tar", "-C", DefaultCNIBinDir, "-xzf", tempFile); err != nil {
		return fmt.Errorf("failed to extract CNI plugins: %w", err)
	}

	logrus.Info("CNI plugins installed successfully")
	return nil
}

func canSkipCNIPluginInstallation() bool {
	for _, plugin := range requiredCNIPlugins {
		pluginPath := filepath.Join(DefaultCNIBinDir, plugin)
		if !utils.FileExistsAndValid(pluginPath) {
			return false
		}
	}
	return true
}

func (i *Installer) constructCNIDownloadURL() (string, string, error) {
	cniVersion := getCNIVersion(i.config)
	arch, err := utils.GetArc()
	if err != nil {
		return "", "", fmt.Errorf("failed to get architecture: %w", err)
	}
	url := fmt.Sprintf(cniDownLoadURL, cniVersion, arch, cniVersion)
	fileName := fmt.Sprintf(cniFileName, arch, cniVersion)
	i.logger.Infof("Constructed CNI download URL: %s", url)
	return fileName, url, nil
}

func getCNIVersion(cfg *config.Config) string {
	if cfg.CNI.Version != "" {
		return cfg.CNI.Version
	}
	return DefaultCNIVersion
}

// CreateBridgeConfig creates bridge CNI configuration for edge nodes (compatible with AKS Cilium)
func (i *Installer) createBridgeConfig() error {
	logrus.Info("Creating bridge CNI configuration for edge node...")
	configPath := filepath.Join(DefaultCNIConfDir, bridgeConfigFile)

	// Load br_netfilter kernel module which is required for bridge networking
	// This enables these sysctl settings:
	// - net.bridge.bridge-nf-call-iptables = 1
	// - net.bridge.bridge-nf-call-ip6tables = 1
	if err := utils.RunSystemCommand("modprobe", "br_netfilter"); err != nil {
		logrus.Warnf("Failed to load br_netfilter module: %v", err)
	}

	// Remove any existing config to start fresh
	if err := utils.RunCleanupCommand(configPath); err != nil {
		logrus.Warnf("Failed to remove existing config file: %v", err)
	}

	bridgeConfig := fmt.Sprintf(`{
    "cniVersion": "%s",
    "name": "bridge",
    "type": "bridge",
    "bridge": "cni0",
    "isGateway": true,
    "ipMasq": true,
    "ipam": {
        "type": "host-local",
        "ranges": [
            [
                {
                    "subnet": "10.244.0.0/16",
                    "gateway": "10.244.0.1"
                }
            ]
        ],
        "routes": [
            {
                "dst": "0.0.0.0/0"
            }
        ]
    }
}`, DefaultCNISpecVersion)

	// Write the config file into a temp file for Atomic file write
	tempBridgeFile, err := utils.CreateTempFile("bridge-cni-*.conf", []byte(bridgeConfig))
	if err != nil {
		return fmt.Errorf("failed to create temporary bridge config file: %w", err)
	}
	defer utils.CleanupTempFile(tempBridgeFile.Name())

	// Copy the temp file to the final location
	if err := utils.RunSystemCommand("cp", tempBridgeFile.Name(), configPath); err != nil {
		return fmt.Errorf("failed to Execute bridge config file: %w", err)
	}

	// Set proper permissions - it needs to be readable by the kubelet and CNI runtime, but only writable by root
	if err := utils.RunSystemCommand("chmod", "644", configPath); err != nil {
		return fmt.Errorf("failed to set bridge config file permissions: %w", err)
	}

	// Set proper ownership to root:root
	if err := utils.RunSystemCommand("chown", "root:root", configPath); err != nil {
		logrus.Warnf("Failed to set ownership for bridge config: %v", err)
	}

	logrus.Info("Bridge CNI configuration created")
	return nil
}
