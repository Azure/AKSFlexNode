package privatecluster

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type CleanupMode string

const (
	// CleanupModeLocal removes node and local components, keeps Gateway for other nodes
	CleanupModeLocal CleanupMode = "local"
	// CleanupModeFull removes all components including Azure resources (Gateway, subnet, NSG, etc.)
	CleanupModeFull CleanupMode = "full"
)

type ScriptRunner struct {
	scriptsDir string
}

func NewScriptRunner(scriptsDir string) *ScriptRunner {
	if scriptsDir == "" {
		candidates := []string{
			"./pkg/privatecluster",
		}
		if execPath, err := os.Executable(); err == nil {
			execDir := filepath.Dir(execPath)
			candidates = append(candidates,
				filepath.Join(execDir, "pkg", "privatecluster"),
				execDir,
			)
		}

		for _, dir := range candidates {
			if _, err := os.Stat(filepath.Join(dir, "private-install.sh")); err == nil {
				scriptsDir = dir
				break
			}
		}
	}
	return &ScriptRunner{scriptsDir: scriptsDir}
}

// RunPrivateInstall executes the private-install.sh script
// Assumes the Private AKS cluster already exists and user has admin permissions
func (r *ScriptRunner) RunPrivateInstall(ctx context.Context, aksResourceID string) error {
	scriptPath := filepath.Join(r.scriptsDir, "private-install.sh")

	// Check if script exists
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return fmt.Errorf("script not found: %s", scriptPath)
	}

	// Execute script with AKS resource ID as argument
	cmd := exec.CommandContext(ctx, "bash", scriptPath, "--aks-resource-id", aksResourceID)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("script execution failed: %w", err)
	}

	return nil
}

// RunPrivateUninstall executes the private-uninstall.sh script
// mode: "local" - remove node and local components, keep Gateway
// mode: "full" - remove all components including Azure resources
// aksResourceID is required for "full" mode
func (r *ScriptRunner) RunPrivateUninstall(ctx context.Context, mode CleanupMode, aksResourceID string) error {
	scriptPath := filepath.Join(r.scriptsDir, "private-uninstall.sh")

	// Check if script exists
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return fmt.Errorf("script not found: %s", scriptPath)
	}

	// Build arguments based on mode
	var args []string
	args = append(args, scriptPath)

	switch mode {
	case CleanupModeLocal:
		args = append(args, "--local")
	case CleanupModeFull:
		if aksResourceID == "" {
			return fmt.Errorf("--aks-resource-id is required for full cleanup mode")
		}
		args = append(args, "--full", "--aks-resource-id", aksResourceID)
	default:
		return fmt.Errorf("invalid cleanup mode: %s (use 'local' or 'full')", mode)
	}

	// Execute script
	cmd := exec.CommandContext(ctx, "bash", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("script execution failed: %w", err)
	}

	return nil
}
