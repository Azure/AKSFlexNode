package v20260301

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"

	"github.com/Azure/AKSFlexNode/pkg/utils"
)

// Constants and variables from the old arc package
const (
	arcInstallScriptURL = "https://gbl.his.arc.azure.com/azcmagent-linux"
)

var (
	// Map role names to role definition IDs
	roleDefinitionIDs = map[string]string{
		"Reader":              "acdd72a7-3385-48ef-bd42-f606fba81ae7",
		"Network Contributor": "4d97b98b-1d4f-4787-a291-c67834d212e7",
		"Contributor":         "b24988ac-6180-42a0-ab88-20f7382dd24c",
		"Azure Kubernetes Service RBAC Cluster Admin": "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b",
		"Azure Kubernetes Service Cluster Admin Role": "0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8",
	}

	// Arc services that may be present
	arcServices = []string{"himdsd", "gcarcservice", "extd"}
)

// RoleAssignment represents a role assignment configuration
type roleAssignment struct {
	roleName string
	scope    string
	roleID   string
}

// Helper methods migrated from the old arc package

func isArcAgentInstalled() bool {
	_, err := exec.LookPath("azcmagent")
	return err == nil
}

func isArcServicesRunning(ctx context.Context) bool {
	if !isArcAgentInstalled() {
		return false
	}

	for _, service := range arcServices {
		if !utils.IsServiceActive(service) {
			return false
		}
	}

	cmd := exec.CommandContext(ctx, "pgrep", "-f", "azcmagent")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func getArcMachineIdentityID(arcMachine *armhybridcompute.Machine) string {
	if arcMachine != nil &&
		arcMachine.Identity != nil &&
		arcMachine.Identity.PrincipalID != nil {
		return *arcMachine.Identity.PrincipalID
	}
	return ""
}

// Arc installation methods

func (a *installArcAction) ensureAuthentication(ctx context.Context) error {
	// Test if we can get a valid credential for Azure operations
	_, err := a.getCredential(ctx)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}
	a.logger.Info("Azure authentication verified successfully")
	return nil
}

func (a *installArcAction) installArcAgentBinary(ctx context.Context) error {
	a.logger.Info("Installing Azure Arc agent binary...")

	// Clean up any existing package state to avoid conflicts
	cmd := exec.CommandContext(ctx, "dpkg", "--purge", "azcmagent")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		a.logger.Debug("No existing azcmagent package to remove")
	}

	// Create temporary directory for installation script
	tempDir, err := os.MkdirTemp("", "arc-install-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tempDir); rmErr != nil {
			a.logger.Debug("Failed to clean up temp directory", "dir", tempDir, "error", rmErr)
		}
	}()

	// Download Arc agent installation script
	installScriptPath := filepath.Join(tempDir, "install_linux_azcmagent.sh")
	a.logger.Info("Downloading Azure Arc agent installation script...")

	if err := a.downloadArcInstallScript(ctx, installScriptPath); err != nil {
		return fmt.Errorf("failed to download Arc installation script: %w", err)
	}

	// Make script executable
	if err := os.Chmod(installScriptPath, 0o755); err != nil {
		return fmt.Errorf("failed to make installation script executable: %w", err)
	}

	// Execute installation script
	a.logger.Info("Running Azure Arc agent installation script...")
	installCmd := exec.CommandContext(ctx, "bash", installScriptPath)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		return fmt.Errorf("failed to install Azure Arc agent: %w", err)
	}

	a.logger.Info("Azure Arc agent binary installed successfully")
	return nil
}

func (a *installArcAction) downloadArcInstallScript(ctx context.Context, destPath string) error {
	// Try curl first
	if _, err := exec.LookPath("curl"); err == nil {
		cmd := exec.CommandContext(ctx, "curl", "-L", "-o", destPath, arcInstallScriptURL)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("curl download failed: %w", err)
		}
		return nil
	}

	// Try wget as fallback
	if _, err := exec.LookPath("wget"); err == nil {
		cmd := exec.CommandContext(ctx, "wget", "-O", destPath, arcInstallScriptURL)
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("wget download failed: %w", err)
		}
		return nil
	}

	return fmt.Errorf("neither curl nor wget is available for downloading Arc installation script")
}

func (a *installArcAction) setUpClients(ctx context.Context, subscriptionID string) error {
	// Use credential chain: Arc MSI -> VM MSI -> Azure CLI
	cred, err := a.getCredential(ctx)
	if err != nil {
		return fmt.Errorf("failed to obtain Azure credential: %w", err)
	}

	// Create Azure SDK clients
	a.hybridComputeMachineClient, err = armhybridcompute.NewMachinesClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create hybrid compute client: %w", err)
	}

	a.mcClient, err = armcontainerservice.NewManagedClustersClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create managed clusters client: %w", err)
	}

	a.roleAssignmentsClient, err = armauthorization.NewRoleAssignmentsClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create role assignments client: %w", err)
	}

	a.logger.Info("Azure SDK clients initialized successfully")
	return nil
}

// getCredential implements credential chain for Arc component
func (a *installArcAction) getCredential(ctx context.Context) (azcore.TokenCredential, error) {
	// Try Arc managed identity first (if already registered)
	if cred, err := a.authProvider.ArcCredential(); err == nil {
		if err := a.testCredential(ctx, cred); err == nil {
			a.logger.Info("Using Arc managed identity credential")
			return cred, nil
		}
		a.logger.Debug("Arc managed identity not available, trying alternatives")
	}

	// Try default managed identity (VM MSI)
	if cred, err := azidentity.NewDefaultAzureCredential(nil); err == nil {
		if err := a.testCredential(ctx, cred); err == nil {
			a.logger.Info("Using default Azure credential (likely VM MSI)")
			return cred, nil
		}
	}

	// Fallback to Azure CLI
	if cred, err := azidentity.NewAzureCLICredential(nil); err == nil {
		if err := a.testCredential(ctx, cred); err == nil {
			a.logger.Info("Using Azure CLI credential")
			return cred, nil
		}
	}

	return nil, fmt.Errorf("no valid Azure credential found - ensure VM has managed identity or Azure CLI is authenticated")
}

// testCredential verifies that a credential can get a valid token
func (a *installArcAction) testCredential(ctx context.Context, cred azcore.TokenCredential) error {
	_, err := a.authProvider.GetAccessToken(ctx, cred)
	return err
}
