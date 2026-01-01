package containerd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/components/cni"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
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
func (ci *Installer) Execute(ctx context.Context) error {
	ci.logger.Infof("Installing containerd version %s", ci.config.Containerd.Version)

	// Check if containerd binary exists
	if utils.FileExists(ContainerdBinaryPath) {
		ci.logger.Info("Existing containerd installation found, cleaning up before reinstallation")
		if err := ci.cleanupExistingInstallation(); err != nil {
			ci.logger.Warnf("Failed to cleanup existing containerd installation: %v", err)
			// Continue anyway - the install command should overwrite
		}
	}

	// Construct download URL
	url := fmt.Sprintf("https://github.com/containerd/containerd/releases/download/v%s/containerd-%s-linux-amd64.tar.gz",
		ci.config.Containerd.Version, ci.config.Containerd.Version)

	// Create temporary directory for download and extraction
	tempDir, err := os.MkdirTemp("", "containerd-install-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()

	tarFile := filepath.Join(tempDir, fmt.Sprintf("containerd-%s-linux-amd64.tar.gz", ci.config.Containerd.Version))
	extractDir := filepath.Join(tempDir, "extract")

	// Download containerd with validation
	ci.logger.Infof("Downloading containerd from %s", url)
	if err := utils.DownloadFile(url, tarFile); err != nil {
		return fmt.Errorf("failed to download containerd from %s: %w", url, err)
	}

	// Verify downloaded file exists and has content
	info, err := os.Stat(tarFile)
	if err != nil {
		return fmt.Errorf("downloaded containerd file not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("downloaded containerd file is empty")
	}
	ci.logger.Infof("Downloaded containerd archive (%d bytes)", info.Size())

	// Create extraction directory
	if err := os.MkdirAll(extractDir, 0755); err != nil {
		return fmt.Errorf("failed to create extraction directory: %w", err)
	}

	// Extract containerd
	ci.logger.Info("Extracting containerd archive")
	if err := utils.RunSystemCommand("tar", "-xzf", tarFile, "-C", extractDir); err != nil {
		return fmt.Errorf("failed to extract containerd archive: %w", err)
	}

	// Verify extraction worked
	binDir := filepath.Join(extractDir, "bin")
	if _, err := os.Stat(binDir); err != nil {
		return fmt.Errorf("containerd extraction failed, bin directory not found: %w", err)
	}

	// Verify containerd binary exists in extracted files
	extractedContainerdPath := filepath.Join(binDir, "containerd")
	if !utils.FileExists(extractedContainerdPath) {
		return fmt.Errorf("containerd binary not found in extracted files")
	}

	// Install containerd binaries with proper permissions
	ci.logger.Infof("Installing containerd binaries to %s", SystemBinDir)

	// Get list of binaries to install
	binFiles, err := os.ReadDir(binDir)
	if err != nil {
		return fmt.Errorf("failed to read extracted bin directory: %w", err)
	}

	for _, file := range binFiles {
		if file.IsDir() {
			continue
		}

		srcPath := filepath.Join(binDir, file.Name())
		dstPath := filepath.Join(SystemBinDir, file.Name())

		ci.logger.Debugf("Installing %s to %s", srcPath, dstPath)

		// Install with proper permissions using install command
		if err := utils.RunSystemCommand("install", "-m", "0755", srcPath, dstPath); err != nil {
			return fmt.Errorf("failed to install %s to %s: %w", srcPath, dstPath, err)
		}
	}

	// Configure containerd service and configuration files
	if err := ci.configure(); err != nil {
		return fmt.Errorf("containerd configuration failed: %w", err)
	}

	// Verify installation
	if err := ci.validateInstallation(); err != nil {
		return fmt.Errorf("containerd installation validation failed: %w", err)
	}

	ci.logger.Infof("containerd version %s installed and configured successfully", ci.config.Containerd.Version)
	return nil
}

// IsCompleted checks if containerd and required plugins are installed
func (ci *Installer) IsCompleted(ctx context.Context) bool {
	// Check if containerd binary exists
	if !utils.FileExists(ContainerdBinaryPath) {
		return false
	}

	// Verify it's the correct version and functional
	return ci.validateConfiguration()
}

// isVersionCorrect checks if the installed containerd version matches the expected version
func (ci *Installer) isVersionCorrect() bool {
	output, err := utils.RunCommandWithOutput(ContainerdBinaryPath, "--version")
	if err != nil {
		ci.logger.Debugf("Failed to get containerd version from %s: %v", ContainerdBinaryPath, err)
		return false
	}

	// Check if version output contains expected version
	versionMatch := strings.Contains(string(output), ci.config.Containerd.Version)
	if !versionMatch {
		ci.logger.Debugf("containerd version mismatch: expected '%s' in output, got: %s", ci.config.Containerd.Version, strings.TrimSpace(output))
	}

	return versionMatch
}

// validateInstallation validates that containerd was installed correctly and is functional
func (ci *Installer) validateInstallation() error {
	// Check if main binary exists
	if !utils.FileExists(ContainerdBinaryPath) {
		return fmt.Errorf("containerd binary not found after installation")
	}

	// Check if binary is executable
	if err := utils.RunSystemCommand(ContainerdBinaryPath, "--version"); err != nil {
		return fmt.Errorf("containerd binary is not executable or functional: %w", err)
	}

	// Verify version matches expected
	if !ci.isVersionCorrect() {
		return fmt.Errorf("installed containerd version does not match expected version %s", ci.config.Containerd.Version)
	}

	// Check for required containerd components
	requiredBinaries := []string{"containerd", "containerd-shim-runc-v2", "ctr"}
	for _, binary := range requiredBinaries {
		binaryPath := filepath.Join(SystemBinDir, binary)
		if !utils.FileExists(binaryPath) {
			ci.logger.Warnf("Optional containerd binary not found: %s", binary)
		} else {
			ci.logger.Debugf("Verified containerd component exists: %s", binary)
		}
	}

	return nil
}

// validateConfiguration validates that containerd configuration is correct and functional
func (ci *Installer) validateConfiguration() bool {
	// Check if containerd binary is executable
	if output, err := utils.RunCommandWithOutput("test", "-x", ContainerdBinaryPath); err != nil {
		ci.logger.Debugf("containerd binary is not executable: %v", err)
		return false
	} else if output != "" {
		ci.logger.Debugf("containerd binary test output: %s", output)
	}

	// Verify containerd version is correct
	if !ci.isVersionCorrect() {
		return false
	}

	// Test basic containerd functionality
	output, err := utils.RunCommandWithOutput(ContainerdBinaryPath, "--version")
	if err != nil {
		ci.logger.Debugf("Failed to run containerd --version: %v", err)
		return false
	}

	// Check that version output contains expected markers
	expectedMarkers := []string{"containerd", ci.config.Containerd.Version}
	for _, marker := range expectedMarkers {
		if !strings.Contains(output, marker) {
			ci.logger.Debugf("Missing expected marker in containerd version output: %s", marker)
			return false
		}
	}

	return true
}

// cleanupExistingInstallation removes any existing containerd installation that may be corrupted
func (ci *Installer) cleanupExistingInstallation() error {
	ci.logger.Debug("Cleaning up existing containerd installation files")

	// Try to stop any processes that might be using containerd (best effort)
	if err := utils.RunSystemCommand("pkill", "-f", "containerd"); err != nil {
		ci.logger.Debugf("No containerd processes found to kill (or pkill failed): %v", err)
	}

	// List of binaries to clean up
	binariesToClean := []string{"containerd", "containerd-shim-runc-v2", "ctr", "containerd-shim", "containerd-shim-runc-v1"}

	for _, binary := range binariesToClean {
		binaryPath := filepath.Join(SystemBinDir, binary)
		if utils.FileExists(binaryPath) {
			ci.logger.Debugf("Removing existing containerd binary: %s", binaryPath)
			if err := utils.RunCleanupCommand(binaryPath); err != nil {
				ci.logger.Warnf("Failed to remove %s: %v", binaryPath, err)
			}
		}
	}

	ci.logger.Debug("Successfully cleaned up existing containerd installation")
	return nil
}

// configure configures containerd service and systemd unit file
func (ci *Installer) configure() error {
	ci.logger.Info("Configuring containerd")

	// Create containerd systemd service
	if err := ci.createContainerdServiceFile(); err != nil {
		return err
	}

	// Create containerd configuration
	if err := ci.createContainerdConfigFile(); err != nil {
		return err
	}

	// Create kubenet template
	if err := ci.createKubenetTemplateFile(); err != nil {
		return err
	}

	// Reload systemd to pick up the new containerd service configuration
	ci.logger.Info("Reloading systemd to pick up containerd configuration changes")
	if err := utils.RunSystemCommand("systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("failed to reload systemd after containerd configuration: %w", err)
	}

	return nil
}

// createContainerdServiceFile creates the containerd systemd service file
func (ci *Installer) createContainerdServiceFile() error {
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

	// Create containerd service file using sudo-aware approach
	tempFile, err := utils.CreateTempFile("containerd-service-*.service", []byte(containerdService))
	if err != nil {
		return fmt.Errorf("failed to create temporary containerd service file: %w", err)
	}
	defer utils.CleanupTempFile(tempFile.Name())

	// Copy the temp file to the final location using sudo
	if err := utils.RunSystemCommand("cp", tempFile.Name(), "/etc/systemd/system/containerd.service"); err != nil {
		return fmt.Errorf("failed to install containerd service file: %w", err)
	}

	// Set proper permissions
	if err := utils.RunSystemCommand("chmod", "644", "/etc/systemd/system/containerd.service"); err != nil {
		return fmt.Errorf("failed to set containerd service file permissions: %w", err)
	}

	return nil
}

// createContainerdConfigFile creates the containerd configuration file
func (ci *Installer) createContainerdConfigFile() error {
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
		conf_template = "/etc/containerd/kubenet_template.conf"
	[plugins."io.containerd.grpc.v1.cri".registry]
		config_path = "/etc/containerd/certs.d"
	[plugins."io.containerd.grpc.v1.cri".registry.headers]
		X-Meta-Source-Client = ["azure/aks"]
[metrics]
	address = "%s"`,
		ci.config.Containerd.PauseImage,
		cni.DefaultCNIBinDir,
		cni.DefaultCNIConfDir,
		ci.config.Containerd.MetricsAddress)

	// Create containerd config file using sudo-aware approach
	tempConfigFile, err := utils.CreateTempFile("containerd-config-*.toml", []byte(containerdConfig))
	if err != nil {
		return fmt.Errorf("failed to create temporary containerd config file: %w", err)
	}
	defer utils.CleanupTempFile(tempConfigFile.Name())

	// Ensure /etc/containerd directory exists
	if err := utils.RunSystemCommand("mkdir", "-p", "/etc/containerd"); err != nil {
		return fmt.Errorf("failed to create containerd config directory: %w", err)
	}

	// Copy the temp file to the final location using sudo
	if err := utils.RunSystemCommand("cp", tempConfigFile.Name(), "/etc/containerd/config.toml"); err != nil {
		return fmt.Errorf("failed to install containerd config file: %w", err)
	}

	// Set proper permissions
	if err := utils.RunSystemCommand("chmod", "644", "/etc/containerd/config.toml"); err != nil {
		return fmt.Errorf("failed to set containerd config file permissions: %w", err)
	}

	return nil
}

// createKubenetTemplateFile creates the kubenet CNI template file
func (ci *Installer) createKubenetTemplateFile() error {
	kubenetTemplate := `{
    "cniVersion": "0.3.1",
    "name": "kubenet",
    "plugins": [{
    "type": "bridge",
    "bridge": "cbr0",
    "mtu": 1500,
    "addIf": "eth0",
    "isGateway": true,
    "ipMasq": false,
    "promiscMode": true,
    "hairpinMode": false,
    "ipam": {
        "type": "host-local",
        "ranges": [{{range $i, $range := .PodCIDRRanges}}{{if $i}}, {{end}}[{"subnet": "{{$range}}"}]{{end}}],
        "routes": [{{range $i, $route := .Routes}}{{if $i}}, {{end}}{"dst": "{{$route}}"}{{end}}]
    }
    },
    {
    "type": "portmap",
    "capabilities": {"portMappings": true},
    "externalSetMarkChain": "KUBE-MARK-MASQ"
    }]
}`

	// Create kubenet template file using sudo-aware approach
	tempTemplateFile, err := utils.CreateTempFile("kubenet-template-*.conf", []byte(kubenetTemplate))
	if err != nil {
		return fmt.Errorf("failed to create temporary kubenet template file: %w", err)
	}
	defer utils.CleanupTempFile(tempTemplateFile.Name())

	// Copy the temp file to the final location using sudo
	if err := utils.RunSystemCommand("cp", tempTemplateFile.Name(), "/etc/containerd/kubenet_template.conf"); err != nil {
		return fmt.Errorf("failed to install kubenet template file: %w", err)
	}

	// Set proper permissions
	if err := utils.RunSystemCommand("chmod", "644", "/etc/containerd/kubenet_template.conf"); err != nil {
		return fmt.Errorf("failed to set kubenet template file permissions: %w", err)
	}

	return nil
}

// Validate validates preconditions before execution
func (ci *Installer) Validate(ctx context.Context) error {
	return nil
}

// GetName returns the step name
func (ci *Installer) GetName() string {
	return "ContainerdInstaller"
}
