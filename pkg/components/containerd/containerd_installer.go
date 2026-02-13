package containerd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/components/cni"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
)

// Installer handles containerd installation operations
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new containerd Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute downloads and installs the containerd container runtime with required plugins
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Step 1: Preparing containerd directories")
	if err := i.prepareContainerdDirectories(); err != nil {
		return fmt.Errorf("failed to prepare containerd directories: %w", err)
	}
	i.logger.Info("Prepared containerd directories successfully")

	i.logger.Infof("Step 2: Downloading and installing containerd version %s", i.getContainerdVersion())
	if err := i.installContainerd(ctx); err != nil {
		return fmt.Errorf("failed to install containerd: %w", err)
	}
	i.logger.Info("containerd binaries installed successfully")

	// Configure containerd service and configuration files
	i.logger.Info("Step 3: Configuring containerd")
	if err := i.configure(); err != nil {
		return fmt.Errorf("containerd configuration failed: %w", err)
	}
	i.logger.Info("containerd configured successfully")

	i.logger.Info("Installer: containerd installed and configured successfully")
	return nil
}

func (i *Installer) prepareContainerdDirectories() error {
	for _, dir := range containerdDirs {
		// Create directory if it doesn't exist
		if !utils.DirectoryExists(dir) {
			if err := utils.RunSystemCommand("mkdir", "-p", dir); err != nil {
				return fmt.Errorf("failed to create containerd directory %s: %w", dir, err)
			}
		}

		// Clean up any existing configurations to start fresh
		if dir == defaultContainerdConfigDir {
			i.logger.Debugf("Cleaning existing containerd configurations in: %s", dir)
			if err := utils.RunSystemCommand("rm", "-rf", dir+"/*"); err != nil {
				return fmt.Errorf("failed to clean containerd configuration directory: %w", err)
			}
		}

		// Set proper permissions
		if err := utils.RunSystemCommand("chmod", "-R", "0755", dir); err != nil {
			logrus.Warnf("Failed to set permissions for containerd directory %s: %v", dir, err)
		}
	}
	return nil
}

func (i *Installer) installContainerd(ctx context.Context) error {
	// Check if we can skip installation
	if i.canSkipContainerdInstallation() {
		i.logger.Info("containerd is already installed and valid, skipping installation")
		return nil
	}

	// Clean up any corrupted installations before proceeding
	i.logger.Info("Cleaning up corrupted containerd installation files to start fresh")
	if err := i.cleanupExistingInstallation(); err != nil {
		i.logger.Warnf("Failed to cleanup existing containerd installation: %v", err)
		// Continue anyway - we'll install fresh
	}

	// Construct download URL
	_, containerdURL, err := i.constructContainerdDownloadURL()
	if err != nil {
		return fmt.Errorf("failed to construct containerd download URL: %w", err)
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, containerdURL) {
		if err != nil {
			return err
		}

		if !strings.HasPrefix(tarFile.Header.Name, "bin/") {
			continue
		}

		fileContent, err := utilio.ReadAll1GiB(tarFile.Body)
		if err != nil {
			return fmt.Errorf("failed to read tar file %q content: %w", tarFile.Header.Name, err)
		}

		fileName := strings.TrimPrefix(tarFile.Header.Name, "bin/")
		targetFilePath := filepath.Join(systemBinDir, fileName)

		i.logger.Debugf("extracting file %q to %q", tarFile.Header.Name, targetFilePath)

		if err := utilio.WriteFile(targetFilePath, fileContent, 0755); err != nil {
			return fmt.Errorf("failed to write file %q: %w", targetFilePath, err)
		}
	}

	return nil
}

func (i *Installer) canSkipContainerdInstallation() bool {
	// Check if containerd binary exists (only check version-appropriate binaries)
	versionBinaries := getContainerdBinariesForVersion(i.getContainerdVersion())
	for _, binary := range versionBinaries {
		binaryPath := filepath.Join(systemBinDir, binary)
		if !utils.FileExists(binaryPath) {
			i.logger.Debugf("containerd binary %s does not exist", binaryPath)
			return false
		}
	}

	// Verify containerd version is correct
	output, err := utils.RunCommandWithOutput(defaultContainerdBinaryDir, "--version")
	if err != nil {
		i.logger.Debugf("Failed to get containerd version from %s: %v", defaultContainerdBinaryDir, err)
		return false
	}
	versionMatch := strings.Contains(string(output), i.getContainerdVersion())
	if versionMatch {
		i.logger.Infof("containerd version %s is already installed", i.getContainerdVersion())
		return true
	}

	return false
}

// constructContainerdDownloadURL constructs the download URL for the specified containerd version
// it returns the file name and URL for downloading containerd
func (i *Installer) constructContainerdDownloadURL() (string, string, error) {
	containerdVersion := i.getContainerdVersion()
	arch, err := utils.GetArc()
	if err != nil {
		return "", "", fmt.Errorf("failed to get architecture: %w", err)
	}
	url := fmt.Sprintf(containerdDownloadURL, containerdVersion, containerdVersion, arch)
	fileName := fmt.Sprintf(containerdFileName, containerdVersion, arch)
	i.logger.Infof("Constructed containerd download URL: %s", url)
	return fileName, url, nil
}

// cleanupExistingInstallation removes any existing containerd installation that may be corrupted
func (i *Installer) cleanupExistingInstallation() error {
	i.logger.Debug("Cleaning up existing containerd installation files")

	// Try to stop any processes that might be using containerd (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "containerd"); err != nil {
		i.logger.Debugf("No containerd processes found to kill (or pkill failed): %v", err)
	}

	// Clean up all possible binaries (including those from older versions)
	// This ensures we remove deprecated binaries when upgrading from 1.x to 2.x
	for _, binary := range getAllContainerdBinaries() {
		binaryPath := filepath.Join(systemBinDir, binary)
		if utils.FileExists(binaryPath) {
			i.logger.Debugf("Removing existing containerd binary: %s", binaryPath)
			if err := utils.RunCleanupCommand(binaryPath); err != nil {
				i.logger.Warnf("Failed to remove %s: %v", binaryPath, err)
			}
		}
	}

	i.logger.Debug("Successfully cleaned up existing containerd installation")
	return nil
}

// configure configures containerd service and systemd unit file
func (i *Installer) configure() error {
	// Create containerd systemd service
	if err := i.createContainerdServiceFile(); err != nil {
		return err
	}

	// Create containerd configuration
	if err := i.createContainerdConfigFile(); err != nil {
		return err
	}

	// Reload systemd to pick up the new containerd service configuration
	i.logger.Info("Reloading systemd to pick up containerd configuration changes")
	if err := utils.RunSystemCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd after containerd configuration: %w", err)
	}

	return nil
}

// createContainerdServiceFile creates the containerd systemd service file
func (i *Installer) createContainerdServiceFile() error {
	containerdService := `[Unit]
Description=containerd container runtime
Documentation=https://containerd.io
After=network.target local-fs.target
[Service]
ExecStartPre=-/sbin/modprobe overlay
ExecStart=/usr/bin/containerd
Type=notify
Delegate=yes
KillMode=process
Restart=always
RestartSec=5
# Having non-zero Limit*s causes performance problems due to accounting overhead
# in the kernel. We recommend using cgroups to do container-local accounting.
LimitNPROC=infinity
LimitCORE=infinity
LimitNOFILE=infinity
# Comment TasksMax if your systemd version does not supports it.
# Only systemd 226 and above support this version.
TasksMax=infinity
OOMScoreAdjust=-999
[Install]
WantedBy=multi-user.target`

	if err := utilio.WriteFile(containerdServiceFile, []byte(containerdService), 0644); err != nil {
		return err
	}

	return nil
}

// createContainerdConfigFile creates the containerd configuration file
func (i *Installer) createContainerdConfigFile() error {
	containerdConfig := fmt.Sprintf(`version = 2
oom_score = 0
[plugins."io.containerd.grpc.v1.cri"]
	sandbox_image = "%s"
	[plugins."io.containerd.grpc.v1.cri".containerd]
		default_runtime_name = "runc"
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
			runtime_type = "io.containerd.runc.v2"
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
			BinaryName = "/usr/bin/runc"
			SystemdCgroup = true
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.untrusted]
			runtime_type = "io.containerd.runc.v2"
		[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.untrusted.options]
			BinaryName = "/usr/bin/runc"
	[plugins."io.containerd.grpc.v1.cri".cni]
		bin_dir = "%s"
		conf_dir = "%s"
	[plugins."io.containerd.grpc.v1.cri".registry]
		config_path = "/etc/containerd/certs.d"
	[plugins."io.containerd.grpc.v1.cri".registry.headers]
		X-Meta-Source-Client = ["azure/aks"]
[metrics]
	address = "%s"`,
		i.getPauseImage(),
		cni.DefaultCNIBinDir,
		cni.DefaultCNIConfDir,
		i.getMetricsAddress())

	if err := utilio.WriteFile(containerdConfigFile, []byte(containerdConfig), 0644); err != nil {
		return err
	}

	return nil
}

// Validate validates preconditions before execution
func (i *Installer) Validate(ctx context.Context) error {
	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "ContainerdInstaller"
}

// IsCompleted checks if containerd and required plugins are installed
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// Check if containerd binaries are installed and functional
	if !i.canSkipContainerdInstallation() {
		return false
	}

	// Check if containerd config file exists
	if !utils.FileExists(containerdConfigFile) {
		return false
	}

	// Check if containerd service file exists
	if !utils.FileExists(containerdServiceFile) {
		return false
	}

	// Verify systemd can parse the service file
	if err := utils.RunSystemCommand("systemctl", "check", "containerd"); err != nil {
		i.logger.Debugf("containerd service file is invalid: %v", err)
		return false
	}

	return true
}

func (i *Installer) getContainerdVersion() string {
	if i.config.Containerd.Version != "" {
		return i.config.Containerd.Version
	}
	// Default to a known stable version if not specified
	return "1.7.20"
}

func (i *Installer) getPauseImage() string {
	if i.config.Containerd.PauseImage != "" {
		return i.config.Containerd.PauseImage
	}
	// Default pause image
	return "mcr.microsoft.com/oss/kubernetes/pause:3.6"
}

func (i *Installer) getMetricsAddress() string {
	if i.config.Containerd.MetricsAddress != "" {
		return i.config.Containerd.MetricsAddress
	}
	// Default metrics address
	return "0.0.0.0:10257"
}
