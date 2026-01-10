package system_configuration

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles system configuration installation
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new system configuration Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute configures system settings including sysctl and resolv.conf
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Configuring system settings")

	// Configure sysctl settings
	if err := i.configureSysctl(); err != nil {
		return fmt.Errorf("failed to configure sysctl settings: %w", err)
	}

	// Configure resolv.conf
	if err := i.configureResolvConf(); err != nil {
		return fmt.Errorf("failed to configure resolv.conf: %w", err)
	}

	i.logger.Info("System configuration completed successfully")
	return nil
}

// IsCompleted checks if system configuration has been applied
func (i *Installer) IsCompleted(ctx context.Context) bool {
	return utils.FileExists(sysctlConfigPath) &&
		utils.FileExists(resolvConfPath)
}

// Validate validates the system configuration installation
func (i *Installer) Validate(ctx context.Context) error {
	return nil
}

// configureSysctl creates and applies sysctl configuration for Kubernetes
func (i *Installer) configureSysctl() error {
	sysctlConfig := `# Kubernetes sysctl settings
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1`

	// Create sysctl directory if it doesn't exist
	if err := utils.RunSystemCommand("mkdir", "-p", sysctlDir); err != nil {
		return fmt.Errorf("failed to create sysctl directory: %w", err)
	}

	// Create temporary file with sysctl configuration
	tempFile, err := utils.CreateTempFile("sysctl-aks-*.conf", []byte(sysctlConfig))
	if err != nil {
		return fmt.Errorf("failed to create temporary sysctl config file: %w", err)
	}
	defer utils.CleanupTempFile(tempFile.Name())

	// Copy to final location
	if err := utils.RunSystemCommand("cp", tempFile.Name(), sysctlConfigPath); err != nil {
		return fmt.Errorf("failed to install sysctl config file: %w", err)
	}

	// Set proper permissions
	if err := utils.RunSystemCommand("chmod", "644", sysctlConfigPath); err != nil {
		return fmt.Errorf("failed to set sysctl config file permissions: %w", err)
	}

	// Apply sysctl settings
	if err := utils.RunSystemCommand("sysctl", "--system"); err != nil {
		return fmt.Errorf("failed to apply sysctl settings: %w", err)
	}

	i.logger.Info("Sysctl configuration applied successfully")
	return nil
}

// configureResolvConf configures DNS resolution
func (i *Installer) configureResolvConf() error {
	// Check if systemd-resolved is managing DNS
	if utils.FileExists(resolvConfSource) {
		// Create symlink to systemd-resolved configuration
		if err := utils.RunSystemCommand("ln", "-sf", resolvConfSource, resolvConfPath); err != nil {
			return fmt.Errorf("failed to configure resolv.conf symlink: %w", err)
		}
		i.logger.Info("Configured resolv.conf to use systemd-resolved")
	} else {
		i.logger.Info("systemd-resolved not available, using existing resolv.conf")
	}

	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "SystemConfigured"
}
