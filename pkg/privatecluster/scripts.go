package privatecluster

import (
	"context"
	"fmt"
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

	options := InstallOptions{
		AKSResourceID: aksResourceID,
		Gateway:       DefaultGatewayConfig(),
		Verbose:       r.verbose,
	}

	installer := NewInstaller(options)
	return installer.Install(ctx)
}

// RunPrivateUninstall executes the private cluster uninstallation using Go implementation
func (r *ScriptRunner) RunPrivateUninstall(ctx context.Context, mode CleanupMode, aksResourceID string) error {
	if mode == CleanupModeFull && aksResourceID == "" {
		return fmt.Errorf("--aks-resource-id is required for full cleanup mode")
	}

	options := UninstallOptions{
		Mode:          mode,
		AKSResourceID: aksResourceID,
	}

	uninstaller := NewUninstaller(options)
	return uninstaller.Uninstall(ctx)
}
