package v20260301

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/google/uuid"

	"github.com/Azure/AKSFlexNode/components/arc"
)

// RBAC assignment methods

func (a *installArcAction) assignRBACRoles(ctx context.Context, arcMachine *armhybridcompute.Machine, spec *arc.InstallArcSpec) error {
	managedIdentityID := getArcMachineIdentityID(arcMachine)
	if managedIdentityID == "" {
		return fmt.Errorf("managed identity ID not found on Arc machine")
	}

	// Track assignment results
	requiredRoles := a.getRoleAssignments(spec)
	var assignmentErrors []error
	for idx, role := range requiredRoles {
		a.logger.Infof("📋 [%d/%d] Assigning role '%s' on scope: %s", idx+1, len(requiredRoles), role.roleName, role.scope)

		if err := a.assignRole(ctx, managedIdentityID, role.roleID, role.scope, role.roleName, spec.GetSubscriptionId()); err != nil {
			a.logger.Errorf("❌ Failed to assign role '%s': %v", role.roleName, err)
			assignmentErrors = append(assignmentErrors, fmt.Errorf("role '%s': %w", role.roleName, err))
		} else {
			a.logger.Infof("✅ Successfully assigned role '%s'", role.roleName)
		}
	}

	if len(assignmentErrors) > 0 {
		a.logger.Errorf("⚠️  RBAC role assignment completed with %d failures", len(assignmentErrors))
		for _, err := range assignmentErrors {
			a.logger.Errorf("   - %v", err)
		}
		return fmt.Errorf("failed to assign %d out of %d RBAC roles", len(assignmentErrors), len(requiredRoles))
	}

	// wait for permissions to propagate
	a.logger.Infof("⏳ Starting permission polling for arc identity with ID: %s (this may take a few minutes)...", managedIdentityID)
	if err := a.waitForPermissions(ctx, managedIdentityID, spec); err != nil {
		a.logger.Errorf("Failed while waiting for RBAC permissions: %v", err)
		return fmt.Errorf("arc bootstrap setup failed while waiting for RBAC permissions: %w", err)
	}

	a.logger.Info("🎉 All RBAC roles assigned successfully!")
	return nil
}

func (a *installArcAction) getRoleAssignments(spec *arc.InstallArcSpec) []roleAssignment {
	subscriptionID := spec.GetSubscriptionId()
	resourceGroup := spec.GetResourceGroup()
	clusterName := spec.GetAksClusterName()

	// Build scope paths
	subscriptionScope := fmt.Sprintf("/subscriptions/%s", subscriptionID)
	resourceGroupScope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscriptionID, resourceGroup)
	clusterScope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s", subscriptionID, resourceGroup, clusterName)

	// Define required role assignments
	assignments := []roleAssignment{
		{
			roleName: "Reader",
			scope:    subscriptionScope,
			roleID:   roleDefinitionIDs["Reader"],
		},
		{
			roleName: "Network Contributor",
			scope:    resourceGroupScope,
			roleID:   roleDefinitionIDs["Network Contributor"],
		},
		{
			roleName: "Contributor",
			scope:    resourceGroupScope,
			roleID:   roleDefinitionIDs["Contributor"],
		},
		{
			roleName: "Azure Kubernetes Service RBAC Cluster Admin",
			scope:    clusterScope,
			roleID:   roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"],
		},
		{
			roleName: "Azure Kubernetes Service Cluster Admin Role",
			scope:    clusterScope,
			roleID:   roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"],
		},
	}

	return assignments
}

func (a *installArcAction) assignRole(
	ctx context.Context, principalID, roleDefinitionID, scope, roleName, subscriptionID string,
) error {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		subscriptionID, roleDefinitionID)

	const (
		maxRetries   = 5
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := min(initialDelay*time.Duration(1<<(attempt-1)), maxDelay)
			a.logger.Infof("⏳ Retrying role assignment after %v (attempt %d/%d)...", delay, attempt+1, maxRetries)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		roleAssignmentName := uuid.New().String()
		a.logger.Debugf("Calling Azure API to create role assignment with ID: %s (attempt %d/%d)", roleAssignmentName, attempt+1, maxRetries)

		// Set PrincipalType to ServicePrincipal for Arc managed identities
		// This helps Azure work around replication delays when the identity was just created
		principalType := armauthorization.PrincipalTypeServicePrincipal
		assignment := armauthorization.RoleAssignmentCreateParameters{
			Properties: &armauthorization.RoleAssignmentProperties{
				PrincipalID:      &principalID,
				RoleDefinitionID: &fullRoleDefinitionID,
				PrincipalType:    &principalType,
			},
		}

		// this create operation is synchronous - we need to wait for the role propagation to take effect afterwards
		if _, err := a.roleAssignmentsClient.Create(ctx, scope, roleAssignmentName, assignment, nil); err != nil {
			lastErr = err
			errStr := err.Error()

			// Check for common error patterns
			if strings.Contains(errStr, "403") || strings.Contains(errStr, "Forbidden") {
				return fmt.Errorf("insufficient permissions to assign roles - ensure the user/service principal has Owner or User Access Administrator role on the target cluster: %w", err)
			}
			if strings.Contains(errStr, "RoleAssignmentExists") {
				a.logger.Info("ℹ️  Role assignment already exists (detected from error)")
				return nil
			}

			// PrincipalNotFound is retriable - likely Azure AD replication delay
			if strings.Contains(errStr, "PrincipalNotFound") {
				a.logger.Warnf("⚠️  Principal not found (Azure AD replication delay) - will retry...")
				// Provide detailed error information on last attempt only
				if attempt == maxRetries-1 {
					a.logger.Errorf("❌ Role assignment creation failed after %d attempts:", maxRetries)
					a.logger.Errorf("   Principal ID: %s", principalID)
					a.logger.Errorf("   Role Name: %s", roleName)
					a.logger.Errorf("   Role Definition ID: %s", fullRoleDefinitionID)
					a.logger.Errorf("   Scope: %s", scope)
					a.logger.Errorf("   Assignment Name: %s", roleAssignmentName)
					a.logger.Errorf("   Azure API Error: %v", err)
				}
				continue // Retry
			}

			// Non-retriable error - log details and return
			a.logger.Errorf("❌ Role assignment creation failed:")
			a.logger.Errorf("   Principal ID: %s", principalID)
			a.logger.Errorf("   Role Name: %s", roleName)
			a.logger.Errorf("   Role Definition ID: %s", fullRoleDefinitionID)
			a.logger.Errorf("   Scope: %s", scope)
			a.logger.Errorf("   Assignment Name: %s", roleAssignmentName)
			a.logger.Errorf("   Azure API Error: %v", err)
			return fmt.Errorf("failed to create role assignment: %s", err)
		}

		// Success
		a.logger.Debugf("✅ Role assignment created successfully")
		return nil
	}

	// Max retries exhausted
	return fmt.Errorf("failed to assign role after %d attempts due to Azure AD replication delay - arc managed identity not found: %w", maxRetries, lastErr)
}

func (a *installArcAction) waitForPermissions(ctx context.Context, managedIdentityID string, spec *arc.InstallArcSpec) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	maxWaitTime := 10 * time.Minute // Maximum wait time
	timeout := time.After(maxWaitTime)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled while waiting for permissions: %w", ctx.Err())
		case <-timeout:
			return fmt.Errorf("timeout after %v waiting for RBAC permissions to be assigned", maxWaitTime)
		case <-ticker.C:
			if hasPermissions, err := a.checkRequiredPermissions(ctx, managedIdentityID, spec); err == nil && hasPermissions {
				a.logger.Info("✅ All required RBAC permissions are now available!")
				return nil
			} else if err != nil {
				a.logger.Warnf("Error while checking permissions: %s", err)
			}
			a.logger.Info("⏳ Some permissions are still missing, will check again in 10 seconds...")
		}
	}
}

func (a *installArcAction) checkRequiredPermissions(ctx context.Context, managedIdentityID string, spec *arc.InstallArcSpec) (bool, error) {
	// For now, we'll do a simple check to see if we can get the Arc machine
	// In a more sophisticated implementation, we could check specific permissions
	_, err := a.getArcMachine(ctx, spec)
	if err != nil {
		// If we can't get the Arc machine, permissions might not be ready yet
		return false, nil
	}

	// If we can get the machine, assume permissions are working
	return true, nil
}
