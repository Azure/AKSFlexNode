package azurevm

import (
	"context"

	"github.com/sirupsen/logrus"
)

// AzureVmUnInstaller handles Azure VM cleanup operations (for VMs running natively in Azure)
type AzureVmUnInstaller struct {
	*base
}

// NewUnInstaller creates a new Azure VM uninstaller
func NewUnInstaller(logger *logrus.Logger) *AzureVmUnInstaller {
	return &AzureVmUnInstaller{
		base: newBase(logger),
	}
}

// GetName returns the cleanup step name
func (u *AzureVmUnInstaller) GetName() string {
	return "AzureVmUnbootstrap"
}

// Validate validates prerequisites for Azure VM cleanup
func (u *AzureVmUnInstaller) Validate(ctx context.Context) error {
	// TODO: Implement validation
	return nil
}

// Execute performs Azure VM cleanup as part of the unbootstrap process
func (u *AzureVmUnInstaller) Execute(ctx context.Context) error {
	// TODO: Implement execution logic
	return nil
}

// IsCompleted checks if Azure VM cleanup has been completed
func (u *AzureVmUnInstaller) IsCompleted(ctx context.Context) bool {
	// TODO: Implement completion check - always returns false to ensure cleanup is attempted
	return false
}
