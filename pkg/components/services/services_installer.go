package services

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles enabling and starting system services
type Installer struct {
	config *config.Config
	logger *logrus.Logger
}

// NewInstaller creates a new services Installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute enables and starts required services (containerd and kubelet)
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Enabling and starting services")

	// Reload systemd
	if err := utils.ReloadSystemd(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}

	// Enable and start containerd
	i.logger.Info("Enabling and starting containerd service")
	if err := utils.EnableAndStartService("containerd"); err != nil {
		i.logger.Errorf("Failed to enable and start containerd: %v", err)
		return fmt.Errorf("failed to enable and start containerd: %w", err)
	}

	// Restart containerd to pick up CNI configuration changes
	i.logger.Info("Restarting containerd service to apply CNI configuration")
	if err := utils.RestartService("containerd"); err != nil {
		i.logger.Errorf("Failed to restart containerd: %v", err)
		return fmt.Errorf("failed to restart containerd for CNI reload: %w", err)
	}

	// Enable and start kubelet
	i.logger.Info("Enabling and starting kubelet service")
	if err := utils.EnableAndStartService("kubelet"); err != nil {
		i.logger.Errorf("Failed to enable and start kubelet: %v", err)
		return fmt.Errorf("failed to enable and start kubelet: %w", err)
	}

	// Wait for kubelet to start and validate it's running properly
	i.logger.Info("Waiting for kubelet to start...")
	if err := utils.WaitForService("kubelet", 30*time.Second, i.logger); err != nil {
		return fmt.Errorf("kubelet failed to start properly: %w", err)
	}

	i.logger.Info("Enabling and starting node-problem-detector service")
	if err := utils.EnableAndStartService("node-problem-detector"); err != nil {
		i.logger.Errorf("Failed to enable and start node-problem-detector: %v", err)
		return fmt.Errorf("failed to enable and start node-problem-detector: %w", err)
	}

	i.logger.Info("All services enabled and started successfully")
	return nil
}

// IsCompleted checks if containerd and kubelet services are enabled and running
func (i *Installer) IsCompleted(ctx context.Context) bool {
	// always return false to ensure services are reenabled each time
	return false
}

// Validate validates prerequisites for enabling services
func (i *Installer) Validate(ctx context.Context) error {
	i.logger.Debug("Validating prerequisites for enabling services")
	// No specific prerequisites for enabling services
	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "ServicesEnabled"
}
