package privatecluster

import (
	"context"
	"fmt"
)

// ToolInstaller handles installation of CLI tools that cannot be replaced by SDK calls.
type ToolInstaller struct {
	logger *Logger
}

// NewToolInstaller creates a new ToolInstaller instance.
func NewToolInstaller(logger *Logger) *ToolInstaller {
	return &ToolInstaller{logger: logger}
}

// InstallAKSCLI installs kubectl and kubelogin via Azure CLI.
func (t *ToolInstaller) InstallAKSCLI(ctx context.Context) error {
	_, err := RunCommand(ctx, "az", "aks", "install-cli",
		"--install-location", "/usr/local/bin/kubectl",
		"--kubelogin-install-location", "/usr/local/bin/kubelogin")
	if err != nil {
		return fmt.Errorf("failed to install kubectl/kubelogin: %w", err)
	}

	_, _ = RunCommand(ctx, "chmod", "+x", "/usr/local/bin/kubectl", "/usr/local/bin/kubelogin")
	return nil
}

// InstallConnectedMachineExtension installs the connectedmachine Azure CLI extension.
func (t *ToolInstaller) InstallConnectedMachineExtension(ctx context.Context) error {
	// Check if already installed
	if RunCommandSilent(ctx, "az", "extension", "show", "--name", "connectedmachine") {
		return nil
	}

	_, _ = RunCommand(ctx, "az", "config", "set", "extension.dynamic_install_allow_preview=true", "--only-show-errors")

	_, err := RunCommand(ctx, "az", "extension", "add",
		"--name", "connectedmachine",
		"--allow-preview", "true",
		"--only-show-errors")
	return err
}
