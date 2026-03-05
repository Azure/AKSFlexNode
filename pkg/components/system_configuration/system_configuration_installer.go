package system_configuration

import (
	"context"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/sirupsen/logrus"
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
	// FIXME: this doesn't make sense to me, so disable for now
	// if err := i.configureResolvConf(); err != nil {
	// 	return fmt.Errorf("failed to configure resolv.conf: %w", err)
	// }

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
	// Disable swap immediately - kubelet sees no active swap devices
	// so it can start successfully. This is a critical step for kubelet compatibility.
	if err := i.disableSwap(); err != nil {
		return fmt.Errorf("failed to disable swap: %w", err)
	}

	sysctlConfig := `# Kubernetes sysctl settings
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1`

	if err := utilio.WriteFile(sysctlConfigPath, []byte(sysctlConfig), 0644); err != nil {
		return err
	}

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

// disableSwap disables swap immediately for kubelet compatibility
func (i *Installer) disableSwap() error {
	i.logger.Info("Disabling swap for kubelet compatibility")

	// Disable all swap devices immediately
	if err := utils.RunSystemCommand("swapoff", "-a"); err != nil {
		i.logger.WithError(err).Warning("Failed to disable swap - may not be enabled")
	} else {
		i.logger.Info("Swap disabled successfully")
	}

	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "SystemConfigured"
}
