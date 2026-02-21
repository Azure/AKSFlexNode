package services

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// KubeletOnlyInstaller enables/starts kubelet without touching containerd.
// This is intended for low-disruption kubelet upgrades.
type KubeletOnlyInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

func NewKubeletOnlyInstaller(logger *logrus.Logger) *KubeletOnlyInstaller {
	return &KubeletOnlyInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

func (k *KubeletOnlyInstaller) GetName() string {
	return "KubeletOnlyEnabled"
}

func (k *KubeletOnlyInstaller) Execute(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	k.logger.Info("Enabling and starting kubelet service (kubelet-only)")
	if err := utils.ReloadSystemd(); err != nil {
		return fmt.Errorf("failed to reload systemd: %w", err)
	}
	if err := utils.EnableAndStartService("kubelet"); err != nil {
		return fmt.Errorf("failed to enable and start kubelet: %w", err)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := utils.WaitForService("kubelet", 30*time.Second, k.logger); err != nil {
		return fmt.Errorf("kubelet failed to start properly: %w", err)
	}
	return nil
}

func (k *KubeletOnlyInstaller) IsCompleted(_ context.Context) bool {
	// Always run to ensure kubelet is restarted after upgrade.
	return false
}
