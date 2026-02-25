package services

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// KubeletOnlyUnInstaller stops and disables kubelet without touching containerd.
// This is intended for low-disruption kubelet upgrades.
type KubeletOnlyUnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

func NewKubeletOnlyUnInstaller(logger *logrus.Logger) *KubeletOnlyUnInstaller {
	return &KubeletOnlyUnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

func (k *KubeletOnlyUnInstaller) GetName() string {
	return "KubeletOnlyDisabled"
}

func (k *KubeletOnlyUnInstaller) Execute(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	k.logger.Info("Stopping and disabling kubelet service (kubelet-only)")
	if utils.ServiceExists("kubelet") {
		if err := utils.StopService("kubelet"); err != nil {
			k.logger.Warnf("Failed to stop kubelet: %v", err)
		}
		if err := utils.DisableService("kubelet"); err != nil {
			k.logger.Warnf("Failed to disable kubelet: %v", err)
		}
	}
	return nil
}

func (k *KubeletOnlyUnInstaller) IsCompleted(_ context.Context) bool {
	return !utils.IsServiceActive("kubelet")
}
