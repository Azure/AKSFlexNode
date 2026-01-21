package npd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

func (i *Installer) GetName() string {
	return "NPD_Installer"
}

func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Infof("Installing Node Problem Detector version %s", i.config.Npd.Version)

	// clean up any existing installation
	if err := i.cleanupExistingInstallation(); err != nil {
		return fmt.Errorf("failed to clean up existing NPD installation: %w", err)
	}

	// Install NPD
	if err := i.installNpd(); err != nil {
		return fmt.Errorf("NPD installation failed: %w", err)
	}

	i.logger.Info("Configuring NPD")
	if err := i.configure(); err != nil {
		return fmt.Errorf("NPD configuration failed: %w", err)
	}

	i.logger.Infof("Node Problem Detector version %s installed successfully", i.config.Npd.Version)
	return nil
}

func (i *Installer) installNpd() error {
	// construct download URL
	npdFileName, npdDownloadURL, err := i.getNpdDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct NPD download URL: %w", err)
	}

	// Clean up any existing NPD files from /tmp directory to avoid conflicts
	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -rf %s", tempDir)); err != nil {
		return fmt.Errorf("failed to clean up existing NPD temp directory %s: %w", tempDir, err)
	}
	defer func() {
		if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -rf %s", tempDir)); err != nil {
			logrus.Warnf("Failed to clean up temp directory %s: %v", tempDir, err)
		}
	}()

	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("mkdir -p %s", tempDir)); err != nil {
		return fmt.Errorf("failed to create NPD temp directory %s: %w", tempDir, err)
	}

	tempFile := fmt.Sprintf("%s/%s", tempDir, npdFileName)

	i.logger.Debugf("Downloading NPD from %s to %s", npdDownloadURL, tempFile)

	if err := utils.DownloadFile(npdDownloadURL, tempFile); err != nil {
		return fmt.Errorf("failed to download NPD archive from %s: %w", npdDownloadURL, err)
	}

	// Extract NPD binary from tar.gz archive
	i.logger.Info("Extracting NPD binary from archive")
	if err := utils.RunSystemCommand("tar", "-xzf", tempFile, "-C", tempDir); err != nil {
		return fmt.Errorf("failed to extract NPD archive: %w", err)
	}

	tempNpdPath := filepath.Join(tempDir, "bin/node-problem-detector")
	tempNpdConfig := filepath.Join(tempDir, "config/kernel-monitor.json")

	// Verify extracted binary
	if output, err := utils.RunCommandWithOutput("file", tempNpdPath); err != nil {
		i.logger.Warnf("Could not verify NPD binary type: %v", err)
	} else {
		i.logger.Debugf("Extracted NPD binary type: %s", strings.TrimSpace(output))
		// Basic validation that it's a Linux binary
		if !strings.Contains(output, "ELF") {
			i.logger.Warnf("Extracted file may not be a Linux binary: %s", output)
		}
	}

	// Install NPD with proper permissions
	i.logger.Infof("Installing NPD binary to %s", npdBinaryPath)
	if err := utils.RunSystemCommand("install", "-m", "0555", tempNpdPath, npdBinaryPath); err != nil {
		return fmt.Errorf("failed to install NPD to %s: %w", npdBinaryPath, err)
	}

	i.logger.Infof("Installing NPD configuration to %s", npdConfigPath)
	if err := utils.RunSystemCommand("install", "-D", "-m", "0644", tempNpdConfig, npdConfigPath); err != nil {
		return fmt.Errorf("failed to install NPD configuration to %s: %w", npdConfigPath, err)
	}

	i.logger.Infof("Node Problem Detector version %s installed successfully", i.config.Npd.Version)
	return nil
}

func (i *Installer) configure() error {
	// Create NPD systemd service
	if err := i.createNpdServiceFile(); err != nil {
		return err
	}

	i.logger.Info("systemd start NPD service")
	if err := utils.RunSystemCommand("systemctl", "start", "node-problem-detector"); err != nil {
		return fmt.Errorf("failed to start NPD service: %w", err)
	}

	return nil
}

func (i *Installer) createNpdServiceFile() error {
	kubeConfigData, err := os.ReadFile(kubeletKubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to read kubelet kubeconfig file: %w", err)
	}

	serverURL, _, err := utils.ExtractClusterInfo(kubeConfigData)
	if err != nil {
		return fmt.Errorf("failed to extract cluster info: %w", err)
	}

	cmd := fmt.Sprintf("%s --apiserver-override=\"%s?inClusterConfig=false&auth=%s\" --config.system-log-monitor=%s",
		npdBinaryPath, serverURL, kubeletKubeconfigPath, npdConfigPath)

	npdService := `[Unit]
Description=Node Problem Detector
After=network.target

[Service]
ExecStart=` + cmd + `
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`
	tempFile, err := utils.CreateTempFile("node-problem-detector-*.service", []byte(npdService))
	if err != nil {
		return fmt.Errorf("failed to create temporary NPD service file: %w", err)
	}
	defer utils.CleanupTempFile(tempFile.Name())

	if err := utils.RunSystemCommand("cp", tempFile.Name(), npdServicePath); err != nil {
		return fmt.Errorf("failed to install NPD service file: %w", err)
	}

	if err := utils.RunSystemCommand("chmod", "644", npdServicePath); err != nil {
		return fmt.Errorf("failed to set NPD service file permissions: %w", err)
	}
	i.logger.Infof("Creating NPD systemd service file at %s", npdServicePath)

	return nil
}

func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if NPD binary exists
	if !utils.FileExists(npdBinaryPath) {
		return false
	}

	// Verify it's the correct version and functional
	return i.isNpdVersionCorrect()
}

// Validate validates prerequisites before installing NPD
func (i *Installer) Validate(ctx context.Context) error {

	return nil
}

// isNpdVersionCorrect checks if the installed NPD version matches the expected version
func (i *Installer) isNpdVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(npdBinaryPath, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get NPD version from %s: %v", npdBinaryPath, err)
		return false
	}

	// Check if version output contains expected version
	versionMatch := strings.Contains(output, i.config.Npd.Version)
	if !versionMatch {
		i.logger.Debugf("NPD version mismatch: expected '%s' in output, got: %s", i.config.Npd.Version, strings.TrimSpace(output))
	}

	return versionMatch
}

// cleanupExistingInstallation removes any existing NPD installation that may be corrupted
func (i *Installer) cleanupExistingInstallation() error {
	i.logger.Debugf("Removing existing NPD binary at %s", npdBinaryPath)

	// Try to stop any processes that might be using NPD (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "node-problem-detector"); err != nil {
		i.logger.Debugf("No NPD processes found to kill (or pkill failed): %v", err)
	}

	// Remove the binary
	if err := utils.RunCleanupCommand(npdBinaryPath); err != nil {
		return fmt.Errorf("failed to remove existing NPD binary at %s: %w", npdBinaryPath, err)
	}

	// Remove the configuration
	if err := utils.RunCleanupCommand(npdConfigPath); err != nil {
		return fmt.Errorf("failed to remove existing NPD configuration at %s: %w", npdConfigPath, err)
	}

	i.logger.Debugf("Successfully cleaned up existing NPD installation")
	return nil
}

func (i *Installer) getNpdDownloadURL() (string, string, error) {
	npdVersion := i.getNpdVersion()
	arch, err := utils.GetArc()
	if err != nil {
		return "", "", fmt.Errorf("failed to get architecture: %w", err)
	}
	// Construct the download URL based on the version
	downloadURL := fmt.Sprintf(npdDownloadURL, npdVersion, npdVersion, arch)
	fileName := fmt.Sprintf(npdFileName, npdVersion)

	return fileName, downloadURL, nil
}

func (i *Installer) getNpdVersion() string {
	if i.config.Npd.Version == "" {
		return "v1.31.1" // default version
	}
	return i.config.Npd.Version
}
