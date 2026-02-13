package npd

import (
	"context"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
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
	if err := i.installNpd(ctx); err != nil {
		return fmt.Errorf("NPD installation failed: %w", err)
	}

	i.logger.Info("Configuring NPD")
	if err := i.configure(); err != nil {
		return fmt.Errorf("NPD configuration failed: %w", err)
	}

	i.logger.Infof("Node Problem Detector version %s installed successfully", i.config.Npd.Version)
	return nil
}

func (i *Installer) installNpd(ctx context.Context) error {
	// construct download URL
	_, npdDownloadURL, err := i.getNpdDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct NPD download URL: %w", err)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, npdDownloadURL) {
		if err != nil {
			return err
		}

		switch n := tarFile.Header.Name; n {
		case "bin/node-problem-detector":
			i.logger.Debugf("installing %q to %q", n, npdBinaryPath)
			if err := utilio.InstallFile(npdBinaryPath, tarFile.Body, 0755); err != nil {
				return fmt.Errorf("failed to install NPD binary: %w", err)
			}
		case "config/kernel-monitor.json":
			i.logger.Debugf("installing %q to %q", n, npdConfigPath)
			if err := utilio.InstallFile(npdConfigPath, tarFile.Body, 0644); err != nil {
				return fmt.Errorf("failed to install NPD config: %w", err)
			}
		default:
			// skip other files in the archive
			continue
		}
	}

	i.logger.Infof("Node Problem Detector version %s installed successfully", i.config.Npd.Version)
	return nil
}

func (i *Installer) configure() error {
	// Create NPD systemd service
	if err := i.createNpdServiceFile(); err != nil {
		return err
	}

	return nil
}

func (i *Installer) createNpdServiceFile() error {
	kubeConfigData, err := utils.RunCommandWithOutput("cat", kubelet.KubeletKubeconfigPath)
	if err != nil {
		return fmt.Errorf("failed to read kubelet kubeconfig file: %w", err)
	}

	serverURL, _, err := utils.ExtractClusterInfo([]byte(kubeConfigData))
	if err != nil {
		return fmt.Errorf("failed to extract cluster info: %w", err)
	}

	cmd := fmt.Sprintf("%s --apiserver-override=\"%s?inClusterConfig=false&auth=%s\" --config.system-log-monitor=%s",
		npdBinaryPath, serverURL, kubelet.KubeletKubeconfigPath, npdConfigPath)

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
	// Write NPD service file atomically with proper permissions
	if err := utilio.WriteFile(npdServicePath, []byte(npdService), 0644); err != nil {
		return fmt.Errorf("failed to create NPD service file: %w", err)
	}

	i.logger.Infof("Created NPD systemd service file at %s", npdServicePath)

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
