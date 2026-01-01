package cni

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles CNI cleanup operations
type UnInstaller struct {
	config *config.Config
	logger *logrus.Logger
}

// NewUnInstaller creates a new CNI setup unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		config: config.GetConfig(),
		logger: logger,
	}
}

// Execute removes CNI configuration directories and files
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Cleaning up CNI configuration")

	// Remove CNI configuration directories
	if dirErrors := utils.RemoveDirectories(cniDirs, u.logger); len(dirErrors) > 0 {
		for _, err := range dirErrors {
			u.logger.Warnf("Directory removal error: %v", err)
		}
	}

	u.logger.Info("CNI configuration cleanup completed")
	return nil
}

// IsCompleted checks if CNI configuration directories have been removed
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	for _, dir := range cniDirs {
		if utils.DirectoryExists(dir) {
			return false
		}
	}

	return true
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "CNICleanup"
}
