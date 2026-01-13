package kube_binaries

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles Kubernetes components removal operations
type UnInstaller struct {
	logger *logrus.Logger
}

// NewUnInstaller creates a new Kubernetes components unInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		logger: logger,
	}
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "KubernetesComponentsExecuteed"
}

// Execute removes Kubernetes components
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Executeing Kubernetes components")

	// List of Kubernetes packages to remove
	packages := []string{
		kubeletBinary,
		kubeadmBinary,
		kubectlBinary,
	}

	// Remove packages using shared utility
	errors := utils.RemovePackages(packages, u.logger)

	// Clean up package cache using shared utility
	utils.CleanPackageCache(u.logger)

	// Remove Kubernetes repository files
	u.logger.Info("Removing Kubernetes apt repository")
	repoFiles := []string{
		KubernetesRepoList,
		KubernetesKeyring,
	}
	utils.RemoveFiles(repoFiles, u.logger)

	// Remove binaries directly if they were manually Executeed
	u.logger.Info("Removing Kubernetes binaries")
	binaryFiles := []string{
		kubeletPath,
		kubectlPath,
		kubeadmPath,
	}
	if fileErrors := utils.RemoveFiles(binaryFiles, u.logger); len(fileErrors) > 0 {
		for _, err := range fileErrors {
			u.logger.Warnf("Binary removal error: %v", err)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("failed to remove %d Kubernetes components: %v", len(errors), errors[0])
	}

	u.logger.Info("Kubernetes components Executeed successfully")
	return nil
}

// IsCompleted checks if Kubernetes components have been removed
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	return !utils.BinaryExists(kubeletBinary)
}
