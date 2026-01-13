package runc

import (
	"context"
	"fmt"
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
	i.logger.Infof("Installing runc version %s", i.getRuncVersion())

	// Clean up any existing stale runc installation
	if err := i.cleanupExistingInstallation(); err != nil {
		return fmt.Errorf("failed to clean up existing runc installation: %w", err)
	}

	// Install runc
	if err := i.installRunc(); err != nil {
		return fmt.Errorf("runc installation failed: %w", err)
	}

	i.logger.Infof("runc version %s installed successfully", i.config.Runc.Version)
	return nil
}

func (i *Installer) installRunc() error {
	// Construct download URL
	runcFileName, runcDownloadURL, err := i.constructRuncDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct runc download URL: %w", err)
	}
	tempFile := fmt.Sprintf("/tmp/%s", runcFileName)

	// Clean up any existing runc temp files from /tmp directory to avoid conflicts
	if err := utils.RunSystemCommand("bash", "-c", fmt.Sprintf("rm -f %s", tempFile)); err != nil {
		logrus.Warnf("Failed to clean up existing runc temp files from /tmp: %s", err)
	}
	defer func() {
		if err := utils.RunCleanupCommand(tempFile); err != nil {
			logrus.Warnf("Failed to clean up temp file %s: %v", tempFile, err)
		}
	}()

	i.logger.Infof("Downloading runc from %s into %s", runcDownloadURL, tempFile)

	if err := utils.DownloadFile(runcDownloadURL, tempFile); err != nil {
		return fmt.Errorf("failed to download runc from %s: %w", runcDownloadURL, err)
	}

	// Install runc with proper permissions
	i.logger.Infof("Installing runc binary to %s", runcBinaryPath)
	if err := utils.RunSystemCommand("install", "-m", "0555", tempFile, runcBinaryPath); err != nil {
		return fmt.Errorf("failed to install runc to %s: %w", runcBinaryPath, err)
	}
	return nil
}

// constructContainerdDownloadURL constructs the download URL for the specified containerd version
// it returns the file name and URL for downloading containerd
func (i *Installer) constructRuncDownloadURL() (string, string, error) {
	runcVersion := i.getRuncVersion()
	arch, err := utils.GetArc()
	if err != nil {
		return "", "", fmt.Errorf("failed to get architecture: %w", err)
	}
	url := fmt.Sprintf(runcDownloadURL, runcVersion, arch)
	fileName := fmt.Sprintf(runcFileName, arch)
	i.logger.Infof("Constructed runc download URL: %s", url)
	return fileName, url, nil
}

// IsCompleted checks if runc is installed and has the correct version
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if runc binary exists
	if !utils.FileExists(runcBinaryPath) {
		return false
	}

	// Verify it's the correct version and functional
	return i.isRuncVersionCorrect()
}

// Validate validates prerequisites before installing runc
func (i *Installer) Validate(ctx context.Context) error {
	return nil
}

// isRuncVersionCorrect checks if the installed runc version matches the expected version
func (i *Installer) isRuncVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(runcBinaryPath, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get runc version from %s: %v", runcBinaryPath, err)
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
	i.logger.Debugf("Removing existing runc binary at %s", runcBinaryPath)

	// Try to stop any processes that might be using runc (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "runc"); err != nil {
		i.logger.Debugf("No runc processes found to kill (or pkill failed): %v", err)
	}

	// Remove the binary
	if err := utils.RunCleanupCommand(runcBinaryPath); err != nil {
		return fmt.Errorf("failed to remove existing runc binary at %s: %w", runcBinaryPath, err)
	}

	i.logger.Debugf("Successfully cleaned up existing runc installation")
	return nil
}

func (i *Installer) getRuncVersion() string {
	if i.config.Runc.Version == "" {
		return "1.1.12" // default version
	}
	return i.config.Runc.Version
}
