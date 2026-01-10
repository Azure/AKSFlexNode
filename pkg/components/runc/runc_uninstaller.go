package runc

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles runc removal
type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

// NewUnInstaller creates a new runc unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (ru *UnInstaller) GetName() string {
	return "Runc_Uninstaller"
}

// Execute removes runc
func (ru *UnInstaller) Execute(ctx context.Context) error {
	ru.logger.Info("Uninstalling runc")

	// Remove runc binary
	if err := utils.RunCleanupCommand(runcBinaryPath); err != nil {
		ru.logger.Debugf("Failed to remove binary %s: %v (may not exist)", runcBinaryPath, err)
	}

	ru.logger.Info("Runc uninstalled successfully")
	return nil
}

// IsCompleted checks if runc has been removed
func (ru *UnInstaller) IsCompleted(ctx context.Context) bool {
	_, err := utils.RunCommandWithOutput("which", "runc")
	return err != nil // runc not found
}
