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
	*Base
}

// NewUnInstaller creates a new Arc UnInstaller
func NewUnInstaller(logger *logrus.Logger) *UnInstaller {
	return &UnInstaller{
		Base: NewBase(logger),
	}
}

// GetName returns the cleanup step name
func (u *UnInstaller) GetName() string {
	return "ArcExecute"
}

// Execute performs Arc cleanup as part of the unbootstrap process
// This method is designed to be called from unbootstrap steps and handles all Arc-related cleanup
// It's resilient to failures and continues cleanup even if some operations fail
func (u *UnInstaller) Execute(ctx context.Context) error {
	u.logger.Info("Starting Arc cleanup for unbootstrap process")

	// Ensure authentication
	if err := u.ensureAuthentication(ctx); err != nil {
		return fmt.Errorf("Arc bootstrap setup failed at authentication: %w", err)
	}

	// Track cleanup operations that failed
	var failedOperations []string

	arcMachine, err := u.getArcMachine(ctx)
	if err != nil {
		u.logger.Infof("Arc machine not found or already unregistered: %v", err)
	}

	// Step 1: Remove RBAC role assignments first (while authentication still works)
	u.logger.Info("Step 1: Removing RBAC role assignments")
	if err := u.removeRBACRoles(ctx, arcMachine); err != nil {
		u.logger.Warnf("Failed to remove RBAC roles (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "RBAC role removal")
	} else {
		u.logger.Info("Successfully removed RBAC role assignments")
	}

	// Step 2: Unregister Arc machine from Azure
	u.logger.Info("Step 2: Unregistering Arc machine from Azure")
	if err := u.unregisterArcMachine(ctx); err != nil {
		u.logger.Warnf("Failed to unregister Arc machine (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "Arc machine unregistration")
	} else {
		u.logger.Info("Successfully unregistered Arc machine from Azure")
	}

	// Step 3: Execute Arc agent from local system
	u.logger.Info("Step 3: Executeing Arc agent from local system")
	if err := u.UninstallArcAgent(ctx); err != nil {
		u.logger.Warnf("Failed to Execute Arc agent (continuing cleanup): %v", err)
		failedOperations = append(failedOperations, "Arc agent Executeation")
	} else {
		u.logger.Info("Successfully Executeed Arc agent")
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

// UninstallArcAgent removes the Azure Arc agent from the system
func (u *UnInstaller) UninstallArcAgent(ctx context.Context) error {
	u.logger.Info("Executeing Azure Arc agent")

	// Check if azcmagent is Executeed
	if _, err := exec.LookPath("azcmagent"); err != nil {
		u.logger.Info("Azure Arc agent is not Executeed or not in PATH")
		return nil
	}

	// Try to disconnect the machine first (if connected)
	if err := u.disconnectArcMachine(ctx); err != nil {
		u.logger.Errorf("Failed to disconnect Arc machine (continuing with Execute): %v", err)
	}

	// Stop Arc agent services
	if err := u.stopArcServices(); err != nil {
		u.logger.Warnf("Failed to stop Arc services (continuing with Execute): %v", err)
	}

	// Remove Arc agent
	if err := u.cleanUpARCAgent(); err != nil {
		u.logger.Debugf("No existing azcmagent package to remove (or removal failed): %v", err)
	}

	u.logger.Info("Azure Arc agent Executeation completed")
	return nil
}

// unregisterArcMachine removes the Arc machine registration from Azure
func (u *UnInstaller) unregisterArcMachine(ctx context.Context) error {
	u.logger.Info("Unregistering Arc machine from Azure")

	// Create hybrid compute machines client
	client, err := u.createHybridComputeClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create hybrid compute client: %w", err)
	}

	arcMachineName := u.config.GetArcMachineName()
	arcResourceGroup := u.config.GetArcResourceGroup()

	u.logger.Infof("Deleting Arc machine resource: %s in resource group: %s", arcMachineName, arcResourceGroup)

	// Delete the Arc machine resource
	_, err = client.Delete(ctx, arcResourceGroup, arcMachineName, nil)
	if err != nil {
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

	// Create role assignments client
	client, err := u.createRoleAssignmentsClient(ctx)
	if err != nil {
		return fmt.Errorf("failed to create role assignments client: %w", err)
	}

	// Define the scopes where we assigned roles
	// Remove each role assignment
	var removalErrors []string
	rolesToRemove := u.getRoleAssignments(arcMachine)
	for _, role := range rolesToRemove {
		u.logger.Infof("Removing role assignment: %s on scope %s", role.RoleName, role.Scope)
		if err := u.removeRoleAssignments(ctx, client, managedIdentityID, role.RoleID, role.Scope, role.RoleName); err != nil {
			u.logger.Warnf("Failed to remove role assignment %s on scope %s: %v", role.RoleName, role.Scope, err)
			removalErrors = append(removalErrors, fmt.Sprintf("%s: %v", role.RoleName, err))
		} else {
			u.logger.Infof("Successfully removed role assignment: %s on scope %s", role.RoleName, role.Scope)
		}
	}

	if len(removalErrors) > 0 {
		return fmt.Errorf("failed to remove some role assignments: %s", strings.Join(removalErrors, "; "))
	}

	u.logger.Info("All RBAC role assignments removed successfully")
	return nil
}

// IsCompleted checks if Arc cleanup has been completed
// This can be used by unbootstrap steps to verify completion status
func (u *UnInstaller) IsCompleted(ctx context.Context) bool {
	u.logger.Debug("Checking Arc cleanup completion status")

	for _, path := range arcPaths {
		if utils.FileExists(path) {
			u.logger.Debugf("Arc agent still found at: %s", path)
			return false
		}
	}

	// Check if Arc services are still running
	if isArcServicesRunning() {
		u.logger.Debug("Arc services are still running")
		return false
	}

	u.logger.Debug("Arc cleanup appears to be completed")
	return true
}

// stopArcServices stops all Arc-related services
func (u *UnInstaller) stopArcServices() error {
	u.logger.Info("Stopping Arc services")

	for _, service := range arcServices {
		u.logger.Debugf("Stopping service: %s", service)
		if err := utils.StopService(service); err != nil {
			u.logger.Debugf("Failed to stop service %s (may not be running): %v", service, err)
		}

		// Disable the service to prevent restart
		if err := utils.DisableService(service); err != nil {
			u.logger.Debugf("Failed to disable service %s: %v", service, err)
		}
	}

	return nil
}

// disconnectArcMachine disconnects the machine using azcmagent
func (u *UnInstaller) disconnectArcMachine(ctx context.Context) error {
	u.logger.Info("Disconnecting Arc machine")

	cmd := exec.CommandContext(ctx, "sudo", "azcmagent", "disconnect", "--force-local-only")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to disconnect Arc machine: %w, output: %s", err, string(output))
	}

	u.logger.Infof("Arc machine disconnected: %s", string(output))
	return nil
}

// removeRoleAssignments removes role assignments for a specific principal, role, and scope
func (u *UnInstaller) removeRoleAssignments(ctx context.Context, client *armauthorization.RoleAssignmentsClient, principalID, roleDefinitionID, scope, roleName string) error {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		u.config.Azure.SubscriptionID, roleDefinitionID)

	// List role assignments for the scope
	pager := client.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
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
		_, err := client.Delete(ctx, scope, assignmentName, nil)
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
