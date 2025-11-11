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

func (ru *UnInstaller) GetName() string {
	return "RuncExecuteed"
}

// Execute removes runc
func (ru *UnInstaller) Execute(ctx context.Context) error {
	ru.logger.Info("Executeing runc")

	// Remove runc package (best effort)
	if err := utils.RunSystemCommand("apt-get", "remove", "-y", "runc"); err != nil {
		ru.logger.Warnf("Failed to remove runc package: %v", err)
	}

	// Remove runc binaries from all possible locations
	for _, binary := range RuncBinaryPaths {
		if err := utils.RunCleanupCommand(binary); err != nil {
			ru.logger.Debugf("Failed to remove binary %s: %v (may not exist)", binary, err)
		} else {
			ru.logger.Infof("Removed binary: %s", binary)
		}
	}

	ru.logger.Info("Runc Executeed successfully")
	return nil
}

// IsCompleted checks if runc has been removed
func (ru *UnInstaller) IsCompleted(ctx context.Context) bool {
	_, err := utils.RunCommandWithOutput("which", "runc")
	return err != nil // runc not found
}
