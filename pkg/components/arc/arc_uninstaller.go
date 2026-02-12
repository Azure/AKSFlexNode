package arc

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// UnInstaller handles Azure Arc cleanup operations
type UnInstaller struct {
	*base
}

// NewUnInstaller creates a new Arc UnInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		base: newBase(logger),
	}
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "ArcUnbootstrap"
}

// IsCompleted checks if Arc cleanup has been completed
// always returns false to ensure cleanup is attempted
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	if !u.config.IsARCEnabled() {
		u.logger.Info("Azure Arc installation is disabled in configuration")
		return true
	}
	return false
}

// Execute performs Arc cleanup as part of the unbootstrap process
// This method is designed to be called from unbootstrap steps and handles all Arc-related cleanup
// It's resilient to failures and continues cleanup even if some operations fail
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Starting Arc cleanup for unbootstrap process")

	// Step 1: Set up Azure SDK clients
	u.logger.Info("Step 1: Setting up Azure SDK clients")
	if err := u.setUpClients(ctx); err != nil {
		return fmt.Errorf("arc bootstrap setup failed at client setup: %w", err)
	}

	arcMachine, err := u.getArcMachine(ctx)
	if err != nil {
		u.logger.Warnf("Failed to get Arc machine (continuing cleanup): %v", err)
	}

	var failedOperations []string
	// Step 2: Remove RBAC role assignments first (while authentication still works)
	u.logger.Info("Step 2: Removing RBAC role assignments")
	if err := u.removeRBACRoles(ctx, arcMachine); err != nil {
		u.logger.Warnf("Failed to remove RBAC roles (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "RBAC role removal")
	} else {
		u.logger.Info("Successfully removed RBAC role assignments")
	}

	// Step 3: Unregister Arc machine resource from Azure
	u.logger.Info("Step 3: Unregistering Arc machine from Azure")
	if err := u.unregisterArcMachine(ctx); err != nil {
		u.logger.Warnf("Failed to unregister Arc machine (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "Arc machine unregistration")
	} else {
		u.logger.Info("Successfully unregistered Arc machine from Azure")
	}

	// Step 4: Disconnect Arc machine
	// It's for local cleanup only: Removes Arc agent state from the local machine
	u.logger.Info("Step 4: Disconnecting Arc machine from Azure (preserving Arc agent)")
	if err := u.disconnectArcMachine(ctx); err != nil {
		u.logger.Warnf("Failed to disconnect Arc machine (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "Arc machine disconnection")
	} else {
		u.logger.Info("Successfully disconnected Arc machine from Azure")
	}

	// Step 5: Remove Arc agent binaries and configuration
	u.logger.Info("Step 5: Removing Arc agent binaries and configuration")
	if err := u.removeArcAgentBinary(ctx); err != nil {
		u.logger.Warnf("Failed to remove Arc agent binaries (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "Arc agent binary removal")
	} else {
		u.logger.Info("Successfully removed Arc agent binaries and configuration")
	}

	// Report results
	if len(failedOperations) > 0 {
		u.logger.Warnf("Arc cleanup completed with %d failed operations: %s",
			len(failedOperations), strings.Join(failedOperations, ", "))
		// Don't return error to allow unbootstrap to continue with other steps
		return nil
	}

	u.logger.Info("Arc cleanup for unbootstrap completed successfully")
	return nil
}

// unregisterArcMachine removes the Arc machine registration from Azure
func (u *UnInstaller) unregisterArcMachine(ctx context.Context) error {
	u.logger.Info("Unregistering Arc machine from Azure")
	arcMachineName := u.config.GetArcMachineName()
	arcResourceGroup := u.config.GetArcResourceGroup()
	u.logger.Infof("Deleting Arc machine resource: %s in resource group: %s", arcMachineName, arcResourceGroup)

	// Delete the Arc machine resource from Azure
	if _, err := u.hybridComputeMachineClient.Delete(ctx, arcResourceGroup, arcMachineName, nil); err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			u.logger.Info("Arc machine resource not found (already deleted)")
			return nil
		}
		return fmt.Errorf("failed to delete Arc machine resource: %w", err)
	}

	u.logger.Info("Arc machine successfully unregistered from Azure")
	return nil
}

// removeRBACRoles removes all RBAC role assignments for the Arc machine's managed identity
func (u *UnInstaller) removeRBACRoles(ctx context.Context, arcMachine *armhybridcompute.Machine) error {
	managedIdentityID := getArcMachineIdentityID(arcMachine)
	if managedIdentityID == "" {
		u.logger.Info("No managed identity found for Arc machine")
		return nil
	}

	u.logger.Infof("Removing role assignments for managed identity: %s", managedIdentityID)

	// Define the scopes where we assigned roles
	// Remove each role assignment
	var removalErrors []string
	rolesToRemove := u.getRoleAssignments()
	for _, role := range rolesToRemove {
		u.logger.Infof("Removing role assignment: %s on scope %s", role.roleName, role.scope)
		if err := u.removeRoleAssignment(ctx, managedIdentityID, role.roleID, role.scope, role.roleName); err != nil {
			u.logger.Warnf("Failed to remove role assignment %s on scope %s: %v", role.roleName, role.scope, err)
			removalErrors = append(removalErrors, fmt.Sprintf("%s: %v", role.roleName, err))
		} else {
			u.logger.Infof("Successfully removed role assignment: %s on scope %s", role.roleName, role.scope)
		}
	}

	if len(removalErrors) > 0 {
		return fmt.Errorf("failed to remove some role assignments: %s", strings.Join(removalErrors, "; "))
	}

	u.logger.Info("All RBAC role assignments removed successfully")
	return nil
}

// disconnectArcMachine disconnects the machine using azcmagent
func (u *UnInstaller) disconnectArcMachine(ctx context.Context) error {
	u.logger.Info("Disconnecting Arc machine")

	cmd := exec.CommandContext(ctx, "azcmagent", "disconnect", "--force-local-only")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to disconnect Arc machine: %w, output: %s", err, string(output))
	}

	u.logger.Infof("Arc machine disconnected: %s", string(output))
	return nil
}

// removeRoleAssignment removes role assignment for a specific principal, role, and scope
func (u *UnInstaller) removeRoleAssignment(ctx context.Context, principalID, roleDefinitionID, scope, roleName string) error {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		u.config.Azure.SubscriptionID, roleDefinitionID)

	// List role assignment for the scope
	pager := u.roleAssignmentsClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
		Filter: nil, // We'll filter programmatically
	})

	var assignmentsToDelete []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("failed to list role assignments for scope %s: %w", scope, err)
		}

		// Find matching role assignments
		for _, assignment := range page.Value {
			if assignment.Properties != nil &&
				assignment.Properties.PrincipalID != nil &&
				assignment.Properties.RoleDefinitionID != nil &&
				*assignment.Properties.PrincipalID == principalID &&
				*assignment.Properties.RoleDefinitionID == fullRoleDefinitionID {
				if assignment.Name != nil {
					assignmentsToDelete = append(assignmentsToDelete, *assignment.Name)
				}
			}
		}
	}

	// Delete found assignments
	for _, assignmentName := range assignmentsToDelete {
		u.logger.Debugf("Deleting role assignment: %s", assignmentName)
		_, err := u.roleAssignmentsClient.Delete(ctx, scope, assignmentName, nil)
		if err != nil {
			if strings.Contains(err.Error(), "RoleAssignmentNotFound") || strings.Contains(err.Error(), "NotFound") {
				u.logger.Debugf("Role assignment %s not found (already deleted)", assignmentName)
				continue
			}
			return fmt.Errorf("failed to delete role assignment %s: %w", assignmentName, err)
		}
		u.logger.Debugf("Successfully deleted role assignment: %s", assignmentName)
	}

	if len(assignmentsToDelete) == 0 {
		u.logger.Debugf("No role assignments found for role %s on scope %s", roleName, scope)
	}

	return nil
}

// removeArcAgentBinary removes Arc agent binaries, services, and configuration files
func (u *UnInstaller) removeArcAgentBinary(ctx context.Context) error {
	u.logger.Info("Removing Azure Arc agent binaries and configuration...")

	if !isArcAgentInstalled() {
		u.logger.Info("Azure Arc agent not found - already removed or never installed")
		return nil
	}

	// Ensure disconnection happened (in case it failed earlier)
	u.logger.Info("Ensuring Arc machine is disconnected...")
	if err := utils.RunSystemCommand("azcmagent", "disconnect", "--force-local-only"); err != nil {
		u.logger.Debug("Arc disconnect command failed or already disconnected")
	}

	// Stop Arc agent services
	u.logger.Info("Stopping Arc agent services...")
	for _, service := range arcServices {
		// Only try to stop/disable services that actually exist
		if utils.ServiceExists(service) {
			if err := utils.StopService(service); err != nil {
				u.logger.Debugf("Failed to stop service %s: %v", service, err)
			}
			if err := utils.DisableService(service); err != nil {
				u.logger.Debugf("Failed to disable service %s: %v", service, err)
			}
		} else {
			u.logger.Debugf("Service %s does not exist, skipping", service)
		}
	}

	// Remove Arc agent binaries and files
	u.logger.Info("Removing Arc agent binaries...")
	for _, path := range arcBinaryPaths {
		if err := utils.RunCleanupCommand(path); err != nil {
			u.logger.Debugf("Failed to remove binary %s: %v", path, err)
		}
	}

	// Clean up Arc directories and configuration
	u.logger.Info("Removing Arc configuration directories...")
	if errors := utils.RemoveDirectories(arcDirectories, u.logger); len(errors) > 0 {
		for _, err := range errors {
			u.logger.Debugf("Directory removal error: %v", err)
		}
	}

	// Remove systemd service files
	u.logger.Info("Removing Arc systemd service files...")
	for _, file := range arcServiceFiles {
		if err := utils.RunCleanupCommand(file); err != nil {
			u.logger.Debugf("Failed to remove service file %s: %v", file, err)
		}
	}

	// Reload systemd
	if err := utils.ReloadSystemd(); err != nil {
		u.logger.Debug("Failed to reload systemd daemon")
	}

	// Verify removal
	if isArcAgentInstalled() {
		u.logger.Warning("azcmagent command still available after cleanup - manual removal may be required")
		return fmt.Errorf("arc agent binary still present after cleanup")
	}

	u.logger.Info("Azure Arc agent binaries and configuration removed successfully")
	return nil
}
