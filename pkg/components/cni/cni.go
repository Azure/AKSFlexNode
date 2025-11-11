package cni

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Manager handles CNI configuration and setup
type Manager struct {
	config *config.Config
}

// NewManager creates a new CNI manager
func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		config: cfg,
	}
}

// SetupCilium configures Cilium CNI for edge nodes
func (c *Manager) SetupCilium() error {
	logrus.Info("Setting up Cilium CNI configuration...")

	// Ensure CNI directories exist
	cniPaths := []string{
		c.config.Paths.CNI.BinDir,
		c.config.Paths.CNI.ConfDir,
		c.config.Paths.CNI.LibDir,
	}

	for _, path := range cniPaths {
		if err := utils.RunSystemCommand("mkdir", "-p", path); err != nil {
			return fmt.Errorf("failed to create CNI directory %s: %w", path, err)
		}
	}

	// Set proper ownership for CNI bin directory
	if err := utils.RunSystemCommand("chown", "-R", "root:root", c.config.Paths.CNI.BinDir); err != nil {
		logrus.Warnf("Failed to set ownership for CNI bin directory: %v", err)
	}

	// Remove any existing Flannel configuration
	if err := c.removeFlannel(); err != nil {
		logrus.Warnf("Failed to remove Flannel configuration: %v", err)
	}

	logrus.Info("Cilium CNI setup completed")
	return nil
}

// removeFlannel removes existing Flannel CNI configuration
func (c *Manager) removeFlannel() error {
	flannelConfigs := []string{
		filepath.Join(c.config.Paths.CNI.ConfDir, "10-flannel.conflist"),
		filepath.Join(c.config.Paths.CNI.ConfDir, "10-flannel.conf"),
	}

	for _, confPath := range flannelConfigs {
		if _, err := os.Stat(confPath); err == nil {
			logrus.Infof("Removing Flannel config: %s", confPath)
			if err := utils.RunCleanupCommand(confPath); err != nil {
				return fmt.Errorf("failed to remove %s: %w", confPath, err)
			}
		}
	}

	return nil
}

// ExecuteCNIPlugins downloads and installs CNI plugins (matching reference script)
func (c *Manager) ExecuteCNIPlugins(version string) error {
	logrus.Infof("Installing CNI plugins version %s...", version)

	// Check if CNI plugins are already installed by looking for common plugins
	// Also verify they have proper file sizes (not corrupted/incomplete installations)
	requiredPlugins := []string{"bridge", "host-local", "loopback"}
	allInstalled := true
	for _, plugin := range requiredPlugins {
		pluginPath := filepath.Join(c.config.Paths.CNI.BinDir, plugin)
		stat, err := os.Stat(pluginPath)
		if err != nil || stat.Size() == 0 {
			// Plugin doesn't exist or is corrupted (0 bytes)
			allInstalled = false
			break
		}
	}

	if allInstalled {
		logrus.Info("CNI plugins are already installed and valid, skipping installation")
		return nil
	}

	// Clean up any corrupted installations before proceeding
	logrus.Info("Cleaning up corrupted CNI plugins before fresh installation")
	if err := utils.RunSystemCommand("rm", "-rf", c.config.Paths.CNI.BinDir+"/*"); err != nil {
		logrus.Warnf("Failed to clean CNI bin directory: %v", err)
	}

	// Check if curl is already installed before trying to install it
	if err := utils.RunSystemCommand("which", "curl"); err != nil {
		logrus.Info("Installing curl...")
		if err := utils.RunSystemCommand("apt", "install", "-y", "curl"); err != nil {
			logrus.Warnf("Failed to install curl: %v", err)
		}
	} else {
		logrus.Info("curl is already installed")
	}

	// Get architecture using same logic as reference script
	arch, err := utils.RunCommandWithOutput("uname", "-m")
	if err != nil {
		return fmt.Errorf("failed to get architecture: %w", err)
	}
	arch = strings.TrimSpace(arch)

	// Map architecture names to match reference script logic
	switch arch {
	case "armv7l", "armv7":
		arch = "arm"
	case "aarch64":
		arch = "arm64"
	case "x86_64":
		arch = "amd64"
	}

	// Ensure CNI bin directory exists using system command
	if err := utils.RunSystemCommand("mkdir", "-p", c.config.Paths.CNI.BinDir); err != nil {
		return fmt.Errorf("failed to create CNI bin directory: %w", err)
	}

	// Download CNI plugins using curl (matching reference script)
	url := fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/v%s/cni-plugins-linux-%s-v%s.tgz", version, arch, version)

	// Use user's home directory instead of /tmp to avoid space issues
	homeDir := os.Getenv("HOME")
	if homeDir == "" {
		homeDir = "/tmp" // fallback to /tmp if HOME not set
	}
	tempFile := fmt.Sprintf("%s/cni-plugins-linux-%s-v%s.tgz", homeDir, arch, version)

	// Clean up any existing CNI temp files from both /tmp and home directory
	// Use shell expansion to handle wildcards properly
	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -f /tmp/cni-plugins-linux-%s-*.tgz", arch)); err != nil {
		logrus.Warnf("Failed to clean up existing CNI temp files from /tmp: %v", err)
	}
	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -f %s/cni-plugins-linux-%s-*.tgz", homeDir, arch)); err != nil {
		logrus.Warnf("Failed to clean up existing CNI temp files from %s: %v", homeDir, err)
	}

	// Also specifically clean up the exact temp file we'll use
	if err := utils.RunCleanupCommand(tempFile); err != nil {
		logrus.Warnf("Failed to clean up specific temp file: %v", err)
	}

	// Check available space after cleanup
	if output, err := utils.RunCommandWithOutput("df", "-h", "/tmp"); err == nil {
		logrus.Infof("Available space in /tmp after cleanup: %s", output)
	}
	if homeDir != "/tmp" {
		if output, err := utils.RunCommandWithOutput("df", "-h", homeDir); err == nil {
			logrus.Infof("Available space in %s for download: %s", homeDir, output)
		}
	}

	// Download to specific temp file location
	if err := utils.RunSystemCommand("curl", "-o", tempFile, "-L", url); err != nil {
		return fmt.Errorf("failed to download CNI plugins: %w", err)
	}
	defer func() {
		if err := utils.RunCleanupCommand(tempFile); err != nil {
			logrus.Warnf("Failed to clean up temp file %s: %v", tempFile, err)
		}
	}()

	// Extract CNI plugins to /opt/cni/bin (matching reference script)
	// The tar command needs sudo to extract files to system directories like /opt/cni/bin
	if err := utils.RunSystemCommand("tar", "-C", c.config.Paths.CNI.BinDir, "-xzf", tempFile); err != nil {
		return fmt.Errorf("failed to extract CNI plugins: %w", err)
	}

	// Load br_netfilter kernel module (critical for DNS connectivity - matching reference script)
	if err := utils.RunSystemCommand("modprobe", "br_netfilter"); err != nil {
		logrus.Warnf("Failed to load br_netfilter module: %v", err)
	}

	logrus.Info("CNI plugins installed successfully with br_netfilter module loaded")
	return nil
}

// CreateBridgeConfig creates bridge CNI configuration for edge nodes (compatible with AKS Cilium)
func (c *Manager) CreateBridgeConfig() error {
	logrus.Info("Creating bridge CNI configuration for edge node...")

	configPath := filepath.Join(c.config.Paths.CNI.ConfDir, "10-bridge.conf")

	// Check if bridge config already exists and validate its content
	if c.isValidBridgeConfig(configPath) {
		logrus.Info("Valid bridge CNI configuration already exists, skipping creation")
		return nil
	}

	// Remove any existing invalid or corrupted config
	if err := utils.RunCleanupCommand(configPath); err != nil {
		logrus.Warnf("Failed to remove existing config file: %v", err)
	}

	// Create a bridge configuration that's compatible with AKS Cilium networking
	bridgeConfig := `{
    "cniVersion": "0.3.1",
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
}`

	// Write the config file with proper permissions using sudo-aware approach
	tempBridgeFile, err := utils.CreateTempFile("bridge-cni-*.conf", []byte(bridgeConfig))
	if err != nil {
		return fmt.Errorf("failed to create temporary bridge config file: %w", err)
	}
	defer utils.CleanupTempFile(tempBridgeFile.Name())

	// Copy the temp file to the final location using sudo
	if err := utils.RunSystemCommand("cp", tempBridgeFile.Name(), configPath); err != nil {
		return fmt.Errorf("failed to Execute bridge config file: %w", err)
	}

	// Set proper permissions
	if err := utils.RunSystemCommand("chmod", "644", configPath); err != nil {
		return fmt.Errorf("failed to set bridge config file permissions: %w", err)
	}

	// Set proper ownership for the config file
	if err := utils.RunSystemCommand("chown", "root:root", configPath); err != nil {
		logrus.Warnf("Failed to set ownership for bridge config: %v", err)
	}

	logrus.Info("Bridge CNI configuration created")
	return nil
}

// ValidateCNI validates CNI configuration
func (c *Manager) ValidateCNI() error {
	logrus.Info("Validating CNI configuration...")

	// Check if CNI directories exist
	if _, err := os.Stat(c.config.Paths.CNI.BinDir); err != nil {
		return fmt.Errorf("CNI bin directory not found: %w", err)
	}

	if _, err := os.Stat(c.config.Paths.CNI.ConfDir); err != nil {
		return fmt.Errorf("CNI conf directory not found: %w", err)
	}

	// Check for CNI plugins
	requiredPlugins := []string{"bridge", "host-local", "loopback", "portmap", "bandwidth", "tuning"}
	for _, plugin := range requiredPlugins {
		pluginPath := filepath.Join(c.config.Paths.CNI.BinDir, plugin)
		if _, err := os.Stat(pluginPath); err != nil {
			logrus.Warnf("CNI plugin %s not found", plugin)
		}
	}

	logrus.Info("CNI validation completed")
	return nil
}

// ValidateBridgeConfig validates the bridge configuration file content
func (c *Manager) ValidateBridgeConfig() error {
	configPath := filepath.Join(c.config.Paths.CNI.ConfDir, "10-bridge.conf")

	if !c.isValidBridgeConfig(configPath) {
		return fmt.Errorf("bridge configuration is invalid or missing")
	}

	return nil
}

// isValidBridgeConfig validates the bridge configuration file content
func (c *Manager) isValidBridgeConfig(configPath string) bool {
	// Check if file exists
	if _, err := os.Stat(configPath); err != nil {
		return false
	}

	// Read and validate the configuration content
	content, err := os.ReadFile(configPath)
	if err != nil {
		logrus.Debugf("Failed to read bridge config file: %v", err)
		return false
	}

	configStr := string(content)

	// Validate essential components exist in the config
	requiredElements := []string{
		`"cniVersion"`,
		`"name": "bridge"`,
		`"type": "bridge"`,
		`"bridge": "cni0"`,
		`"ipam"`,
		`"host-local"`,
		`"10.244.0.0/16"`,
	}

	for _, element := range requiredElements {
		if !strings.Contains(configStr, element) {
			logrus.Debugf("Bridge config missing required element: %s", element)
			return false
		}
	}

	logrus.Debug("Bridge config validation passed")
	return true
}
