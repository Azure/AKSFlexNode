package services

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles stopping and disabling system services
type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

// NewUnInstaller creates a new services unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (su *UnInstaller) GetName() string {
	return "ServicesDisabled"
}

// Execute stops and disables services
func (su *UnInstaller) Execute(ctx context.Context) error {
	su.logger.Info("Stopping and disabling services")

	// Stop and disable kube-proxy
	if utils.ServiceExists("kube-proxy") {
		su.logger.Info("Stopping and disabling kube-proxy service")
		if err := utils.StopService("kube-proxy"); err != nil {
			su.logger.Warnf("Failed to stop kube-proxy: %v", err)
		}
		if err := utils.DisableService("kube-proxy"); err != nil {
			su.logger.Warnf("Failed to disable kube-proxy: %v", err)
		}
	}

	// Stop and disable kubelet
	if utils.ServiceExists("kubelet") {
		su.logger.Info("Stopping and disabling kubelet service")
		if err := utils.StopService("kubelet"); err != nil {
			su.logger.Warnf("Failed to stop kubelet: %v", err)
		}
		if err := utils.DisableService("kubelet"); err != nil {
			su.logger.Warnf("Failed to disable kubelet: %v", err)
		}
	}

	// Stop and disable containerd
	if utils.ServiceExists("containerd") {
		su.logger.Info("Stopping and disabling containerd service")
		if err := utils.StopService("containerd"); err != nil {
			su.logger.Warnf("Failed to stop containerd: %v", err)
		}
		if err := utils.DisableService("containerd"); err != nil {
			su.logger.Warnf("Failed to disable containerd: %v", err)
		}
	}

	su.logger.Info("Services stopped and disabled successfully")
	return nil
}

// IsCompleted checks if services have been stopped and disabled
func (su *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Services are considered completed if they are not active
	return !utils.IsServiceActive("containerd") &&
		!utils.IsServiceActive("kubelet") &&
		!utils.IsServiceActive("kube-proxy")
}
