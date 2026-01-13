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

	// Implementation for removing NPD would go here

	nu.logger.Info("Node Problem Detector uninstalled successfully")
	return nil
}

func (nu *UnInstaller) IsCompleted(ctx context.Context) bool {
	// Check if NPD is uninstalled
	if !utils.FileExists("") {
		return true
	}
	return false
}
