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

	// Create temporary directory for download to avoid conflicts
	tempDir, err := os.MkdirTemp("", "npd-install-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tarFile := filepath.Join(tempDir, "npd.tar.gz")

	// construct download URL
	i.config.Npd.URL = fmt.Sprintf(i.config.Npd.URL, i.config.Npd.Version, i.config.Npd.Version)

	// Download NPD tar.gz archive with validation
	i.logger.Infof("Downloading NPD archive from %s", i.config.Npd.URL)
	if err := utils.DownloadFile(i.config.Npd.URL, tarFile); err != nil {
		return fmt.Errorf("failed to download NPD archive from %s: %w", i.config.Npd.URL, err)
	}

	// Verify downloaded file exists and has content
	info, err := os.Stat(tarFile)
	if err != nil {
		return fmt.Errorf("downloaded NPD archive not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("downloaded NPD archive is empty")
	}
	i.logger.Infof("Downloaded NPD archive (%d bytes)", info.Size())

	// Change to temp directory for extraction
	originalDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current directory: %w", err)
	}
	defer func() {
		_ = os.Chdir(originalDir)
	}()

	if err := os.Chdir(tempDir); err != nil {
		return fmt.Errorf("failed to change to temp directory: %w", err)
	}

	// Extract NPD binary from tar.gz archive
	i.logger.Info("Extracting NPD binary from archive")
	if err := utils.RunSystemCommand("tar", "-xzf", filepath.Base(tarFile)); err != nil {
		return fmt.Errorf("failed to extract NPD archive: %w", err)
	}

	tempNpdPath := filepath.Join(tempDir, "bin/node-problem-detector")
	tempNpdConfig := filepath.Join(tempDir, "config/system-stats-monitor.json")

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
	i.logger.Infof("Installing NPD binary to %s", PrimaryNpdBinaryPath)
	if err := utils.RunSystemCommand("install", "-m", "0555", tempNpdPath, PrimaryNpdBinaryPath); err != nil {
		return fmt.Errorf("failed to install NPD to %s: %w", PrimaryNpdBinaryPath, err)
	}

	i.logger.Infof("Installing NPD configuration to %s", PrimaryNpdConfigPath)
	if err := utils.RunSystemCommand("install", "-D", "0644", tempNpdConfig, PrimaryNpdConfigPath); err != nil {
		return fmt.Errorf("failed to install NPD configuration to %s: %w", PrimaryNpdConfigPath, err)
	}

	i.logger.Infof("Node Problem Detector version %s installed successfully", i.config.Npd.Version)
	return nil
}

func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if NPD binary exists
	if !utils.FileExists(PrimaryNpdBinaryPath) {
		return false
	}

	// Verify it's the correct version and functional
	return i.isNpdVersionCorrect()
}

// Validate validates prerequisites before installing NPD
func (i *Installer) Validate(ctx context.Context) error {
	i.logger.Debug("Validating prerequisites for NPD installation")

	// Clean up any existing corrupted installation before proceeding
	if utils.FileExists(PrimaryNpdBinaryPath) {
		i.logger.Info("Existing NPD installation found, cleaning up before reinstallation")
		if err := i.cleanupExistingInstallation(); err != nil {
			i.logger.Warnf("Failed to cleanup existing NPD installation: %v", err)
			// Continue anyway - the install command should overwrite
		}
	}

	return nil
}

// isNpdVersionCorrect checks if the installed NPD version matches the expected version
func (i *Installer) isNpdVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(PrimaryNpdBinaryPath, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get NPD version from %s: %v", PrimaryNpdBinaryPath, err)
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
	i.logger.Debugf("Removing existing NPD binary at %s", PrimaryNpdBinaryPath)

	// Try to stop any processes that might be using NPD (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "node-problem-detector"); err != nil {
		i.logger.Debugf("No NPD processes found to kill (or pkill failed): %v", err)
	}

	// Remove the binary
	if err := utils.RunCleanupCommand(PrimaryNpdBinaryPath); err != nil {
		return fmt.Errorf("failed to remove existing NPD binary at %s: %w", PrimaryNpdBinaryPath, err)
	}

	// Remove the configuration
	if err := utils.RunCleanupCommand(PrimaryNpdConfigPath); err != nil {
		return fmt.Errorf("failed to remove existing NPD configuration at %s: %w", PrimaryNpdConfigPath, err)
	}

	i.logger.Debugf("Successfully cleaned up existing NPD installation")
	return nil
}
