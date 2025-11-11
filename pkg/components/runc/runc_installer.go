package runc

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

// Installer handles runc container runtime installation
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new runc Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "Runc_Installer"
}

// Execute downloads and installs the runc container runtime
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Infof("Installing runc version %s", i.config.Runc.Version)

	// Create temporary directory for download to avoid conflicts
	tempDir, err := os.MkdirTemp("", "runc-install-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)

	tempRuncPath := filepath.Join(tempDir, "runc")

	// Download runc with validation
	i.logger.Infof("Downloading runc from %s", i.config.Runc.URL)
	if err := utils.DownloadFile(i.config.Runc.URL, tempRuncPath); err != nil {
		return fmt.Errorf("failed to download runc from %s: %w", i.config.Runc.URL, err)
	}

	// Verify downloaded file exists and has content
	info, err := os.Stat(tempRuncPath)
	if err != nil {
		return fmt.Errorf("downloaded runc file not found at %s: %w", tempRuncPath, err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("downloaded runc file is empty")
	}
	i.logger.Infof("Downloaded runc binary (%d bytes)", info.Size())

	// Verify file is executable binary (basic check)
	if output, err := utils.RunCommandWithOutput("file", tempRuncPath); err != nil {
		i.logger.Warnf("Could not verify runc binary type: %v", err)
	} else {
		i.logger.Debugf("Downloaded file type: %s", strings.TrimSpace(output))
		// Basic validation that it's a Linux binary
		if !strings.Contains(output, "ELF") {
			i.logger.Warnf("Downloaded file may not be a Linux binary: %s", output)
		}
	}

	// Install runc with proper permissions
	i.logger.Infof("Installing runc binary to %s", PrimaryRuncBinaryPath)
	if err := utils.RunSystemCommand("install", "-m", "0555", tempRuncPath, PrimaryRuncBinaryPath); err != nil {
		return fmt.Errorf("failed to install runc to %s: %w", PrimaryRuncBinaryPath, err)
	}

	i.logger.Infof("runc version %s installed successfully", i.config.Runc.Version)
	return nil
}

// IsCompleted checks if runc is installed and has the correct version
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if runc binary exists
	if !utils.FileExists(PrimaryRuncBinaryPath) {
		return false
	}

	// Verify it's the correct version and functional
	return i.isRuncVersionCorrect()
}

// Validate validates prerequisites before installing runc
func (i *Installer) Validate(ctx context.Context) error {
	i.logger.Debug("Validating prerequisites for runc installation")

	// Clean up any existing corrupted installation before proceeding
	if utils.FileExists(PrimaryRuncBinaryPath) {
		i.logger.Info("Existing runc installation found, cleaning up before reinstallation")
		if err := i.cleanupExistingInstallation(); err != nil {
			i.logger.Warnf("Failed to cleanup existing runc installation: %v", err)
			// Continue anyway - the install command should overwrite
		}
	}

	return nil
}

// isRuncVersionCorrect checks if the installed runc version matches the expected version
func (i *Installer) isRuncVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(PrimaryRuncBinaryPath, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get runc version from %s: %v", PrimaryRuncBinaryPath, err)
		return false
	}

	// Check if version output contains expected version
	versionMatch := strings.Contains(output, i.config.Runc.Version)
	if !versionMatch {
		i.logger.Debugf("runc version mismatch: expected '%s' in output, got: %s", i.config.Runc.Version, strings.TrimSpace(output))
	}

	return versionMatch
}

// cleanupExistingInstallation removes any existing runc installation that may be corrupted
func (i *Installer) cleanupExistingInstallation() error {
	i.logger.Debugf("Removing existing runc binary at %s", PrimaryRuncBinaryPath)

	// Try to stop any processes that might be using runc (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "runc"); err != nil {
		i.logger.Debugf("No runc processes found to kill (or pkill failed): %v", err)
	}

	// Remove the binary
	if err := utils.RunCleanupCommand(PrimaryRuncBinaryPath); err != nil {
		return fmt.Errorf("failed to remove existing runc binary at %s: %w", PrimaryRuncBinaryPath, err)
	}

	i.logger.Debugf("Successfully cleaned up existing runc installation")
	return nil
}
