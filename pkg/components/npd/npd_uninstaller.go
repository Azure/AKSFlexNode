package npd

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

func (nu *UnInstaller) GetName() string {
	return "NPD_UnInstaller"
}

func (nu *UnInstaller) Execute(ctx context.Context) error {
	nu.logger.Info("Uninstalling Node Problem Detector")

	// Remove npd binary
	if err := utils.RunCleanupCommand(PrimaryNpdBinaryPath); err != nil {
		nu.logger.Debugf("Failed to remove binary %s: %v (may not exist)", PrimaryNpdBinaryPath, err)
	}

	if err := utils.RunCleanupCommand(PrimaryNpdConfigPath); err != nil {
		nu.logger.Debugf("Failed to remove config %s: %v (may not exist)", PrimaryNpdConfigPath, err)
	}

	nu.logger.Info("Node Problem Detector uninstalled successfully")
	return nil
}

func (nu *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Check if NPD is uninstalled
	if !utils.FileExists(PrimaryNpdBinaryPath) && !utils.FileExists(PrimaryNpdConfigPath) {
		return true
	}
	return false
}
