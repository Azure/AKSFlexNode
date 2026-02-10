package privatecluster

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// ScriptRunner provides backward compatibility (Deprecated: use Installer/Uninstaller directly)
type ScriptRunner struct {
	verbose bool
}

// NewScriptRunner creates a new ScriptRunner instance (Deprecated)
func NewScriptRunner(scriptsDir string) *ScriptRunner {
	return &ScriptRunner{verbose: false}
}

// RunPrivateInstall executes the private cluster installation using Go implementation
func (r *ScriptRunner) RunPrivateInstall(ctx context.Context, aksResourceID string) error {
	if aksResourceID == "" {
		return fmt.Errorf("AKS resource ID is required")
	}

	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return fmt.Errorf("failed to create Azure CLI credential: %w", err)
	}

	options := InstallOptions{
		AKSResourceID: aksResourceID,
		Gateway:       DefaultGatewayConfig(),
		Verbose:       r.verbose,
	}

	installer, err := NewInstaller(options, cred)
	if err != nil {
		return fmt.Errorf("failed to create installer: %w", err)
	}
	return installer.Install(ctx)
}

// RunPrivateUninstall executes the private cluster uninstallation using Go implementation
func (r *ScriptRunner) RunPrivateUninstall(ctx context.Context, mode CleanupMode, aksResourceID string) error {
	if mode == CleanupModeFull && aksResourceID == "" {
		return fmt.Errorf("--aks-resource-id is required for full cleanup mode")
	}

	var cred *azidentity.AzureCLICredential
	if aksResourceID != "" {
		var err error
		cred, err = azidentity.NewAzureCLICredential(nil)
		if err != nil {
			return fmt.Errorf("failed to create Azure CLI credential: %w", err)
		}
	}

	options := UninstallOptions{
		Mode:          mode,
		AKSResourceID: aksResourceID,
	}

	uninstaller, err := NewUninstaller(options, cred)
	if err != nil {
		return fmt.Errorf("failed to create uninstaller: %w", err)
	}
	return uninstaller.Uninstall(ctx)
}
