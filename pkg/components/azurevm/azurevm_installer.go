package azurevm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// AzureVmInstaller handles Azure VM installation operations (for VMs running natively in Azure)
type AzureVmInstaller struct {
	*base
}

// NewInstaller creates a new Azure VM installer
func NewInstaller(logger *logrus.Logger) *AzureVmInstaller {
	return &AzureVmInstaller{
		base: newBase(logger),
	}
}

// GetName returns the step name
func (i *AzureVmInstaller) GetName() string {
	return "AzureVmInstall"
}

// Validate validates prerequisites for Azure VM installation
func (i *AzureVmInstaller) Validate(ctx context.Context) error {
	// Ensure SP or CLI auth is ready for Azure VM setup
	if err := i.ensureAuthentication(ctx); err != nil {
		i.logger.Errorf("Authentication setup failed: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed at authentication: %w", err)
	}

	// Validate managed identity is assigned to the VM
	if err := i.validateManagedIdentity(ctx); err != nil {
		i.logger.Errorf("Managed identity validation failed: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed at managed identity validation: %w", err)
	}

	return nil
}

// Execute performs Azure VM setup as part of the bootstrap process
// This method is designed to be called from bootstrap steps and handles all Azure VM-related setup
// It stops on the first error to prevent partial setups
func (i *AzureVmInstaller) Execute(ctx context.Context) error {
	i.logger.Info("Starting Azure VM setup for bootstrap process")

	// Step 1: Set up Azure SDK clients
	if err := i.setUpClients(ctx); err != nil {
		return fmt.Errorf("azure vm bootstrap setup failed at client setup: %w", err)
	}

	// Step 2: Get managed identity from Azure VM IMDS
	i.logger.Info("Step 2: Getting managed identity from Azure VM")
	managedIdentityID, err := i.getManagedIdentityID(ctx)
	if err != nil {
		i.logger.Errorf("Failed to get managed identity: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed at getting managed identity: %w", err)
	}
	i.logger.Infof("Successfully retrieved managed identity ID: %s", managedIdentityID)

	// Step 3: Validate managed cluster requirements
	i.logger.Info("Step 3: Validating Managed Cluster requirements")
	if err := i.validateManagedCluster(ctx); err != nil {
		i.logger.Errorf("Managed Cluster validation failed: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed at managed cluster validation: %w", err)
	}

	// Step 4: Assign RBAC roles to managed identity
	i.logger.Info("Step 4: Assigning RBAC roles to managed identity")
	if err := i.assignRBACRoles(ctx, managedIdentityID); err != nil {
		i.logger.Errorf("Failed to assign RBAC roles: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed at RBAC role assignment: %w", err)
	}
	i.logger.Info("Successfully assigned RBAC roles")

	i.logger.Info("Azure VM setup for bootstrap completed successfully")
	return nil
}

// IsCompleted checks if Azure VM setup has been completed
func (i *AzureVmInstaller) IsCompleted(ctx context.Context) bool {
	return false
}

// validateManagedIdentity checks that the VM has a managed identity assigned and validates configuration
func (i *AzureVmInstaller) validateManagedIdentity(ctx context.Context) error {
	i.logger.Info("Validating managed identity configuration")

	// Step 1: Query IMDS instance endpoint to get VM resource ID
	const imdsInstanceURL = "http://169.254.169.254/metadata/instance?api-version=2025-04-07"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsInstanceURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create IMDS request: %w", err)
	}

	req.Header.Set("Metadata", "true")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to query IMDS instance endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("IMDS instance request failed (status %d): %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read IMDS response: %w", err)
	}

	// Parse the instance response to get resource ID
	var instanceInfo imdsInstanceInfo
	if err := json.Unmarshal(body, &instanceInfo); err != nil {
		return fmt.Errorf("failed to parse IMDS instance response: %w", err)
	}

	resourceID := instanceInfo.Compute.ResourceID
	if resourceID == "" {
		return fmt.Errorf("failed to get VM resource ID from IMDS instance endpoint")
	}

	i.logger.Infof("Found VM resource ID: %s", resourceID)

	// Step 2: Parse resource ID to extract subscription, resource group, and VM name
	subscriptionID, resourceGroup, vmName, err := parseVMResourceID(resourceID)
	if err != nil {
		return fmt.Errorf("failed to parse VM resource ID: %w", err)
	}

	i.logger.Debugf("Parsed VM info - Subscription: %s, ResourceGroup: %s, VMName: %s", subscriptionID, resourceGroup, vmName)

	// Step 3: Set up clients and make ARM call to get VM details
	if err := i.setUpClients(ctx); err != nil {
		return fmt.Errorf("failed to set up Azure clients: %w", err)
	}

	vmResp, err := i.vmClient.Get(ctx, resourceGroup, vmName, nil)
	if err != nil {
		return fmt.Errorf("failed to get VM details from ARM: %w", err)
	}

	// Step 4: Validate identity configuration from VM resource
	vm := vmResp.VirtualMachine
	if vm.Identity == nil {
		return fmt.Errorf("no managed identity found on this Azure VM - please assign a managed identity to the VM")
	}

	hasSystemAssigned := vm.Identity.Type != nil &&
		(*vm.Identity.Type == "SystemAssigned" || *vm.Identity.Type == "SystemAssigned, UserAssigned")
	userAssignedCount := len(vm.Identity.UserAssignedIdentities)

	// Check that at least one identity is assigned
	if !hasSystemAssigned && userAssignedCount == 0 {
		return fmt.Errorf("no managed identity (system or user assigned) found on this Azure VM - please assign a managed identity to the VM")
	}

	i.logger.Infof("Found managed identities: system-assigned=%v, user-assigned count=%d", hasSystemAssigned, userAssignedCount)

	// If more than one user-assigned identity, require explicit configuration
	if userAssignedCount > 1 {
		configuredClientID := ""
		if i.config.Azure.AzureVm != nil &&
			i.config.Azure.AzureVm.ManagedIdentity != nil {
			configuredClientID = i.config.Azure.AzureVm.ManagedIdentity.ClientID
		}

		if configuredClientID == "" {
			return fmt.Errorf("multiple user-assigned managed identities found (%d) - please specify which one to use by setting azure.azureVm.managedIdentity.clientId in the configuration", userAssignedCount)
		}

		// Verify the configured client ID matches one of the assigned identities
		found := false
		for _, identity := range vm.Identity.UserAssignedIdentities {
			if identity != nil && identity.ClientID != nil && *identity.ClientID == configuredClientID {
				found = true
				break
			}
		}

		if !found {
			return fmt.Errorf("configured managed identity client ID '%s' does not match any of the %d user-assigned managed identities on this VM", configuredClientID, userAssignedCount)
		}

		i.logger.Infof("Configured client ID '%s' matches a user-assigned managed identity", configuredClientID)
	}

	i.logger.Info("Managed identity validation successful")
	return nil
}

// imdsInstanceInfo represents the instance information from the IMDS instance endpoint
type imdsInstanceInfo struct {
	Compute struct {
		ResourceID string `json:"resourceId"`
	} `json:"compute"`
}

// parseVMResourceID parses an Azure VM resource ID and extracts subscription, resource group, and VM name
func parseVMResourceID(resourceID string) (subscriptionID, resourceGroup, vmName string, err error) {
	// Expected format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Compute/virtualMachines/{vmName}
	parts := strings.Split(resourceID, "/")
	if len(parts) < 9 {
		return "", "", "", fmt.Errorf("invalid VM resource ID format: %s", resourceID)
	}

	for i, part := range parts {
		switch strings.ToLower(part) {
		case "subscriptions":
			if i+1 < len(parts) {
				subscriptionID = parts[i+1]
			}
		case "resourcegroups":
			if i+1 < len(parts) {
				resourceGroup = parts[i+1]
			}
		case "virtualmachines":
			if i+1 < len(parts) {
				vmName = parts[i+1]
			}
		}
	}

	if subscriptionID == "" || resourceGroup == "" || vmName == "" {
		return "", "", "", fmt.Errorf("failed to extract subscription, resource group, or VM name from resource ID: %s", resourceID)
	}

	return subscriptionID, resourceGroup, vmName, nil
}

// getManagedIdentityID fetches the managed identity details from the Azure VM
// and returns the client ID of the first user or system assigned managed identity found on the VM.
// If the config specifies a client ID, it uses that; otherwise it queries IMDS instance and ARM.
func (i *AzureVmInstaller) getManagedIdentityID(ctx context.Context) (string, error) {
	// Check if a specific client ID is configured
	if i.config.Azure.AzureVm != nil &&
		i.config.Azure.AzureVm.ManagedIdentity != nil &&
		i.config.Azure.AzureVm.ManagedIdentity.ClientID != "" {
		i.logger.Info("Using configured managed identity client ID")
		return i.config.Azure.AzureVm.ManagedIdentity.ClientID, nil
	}

	i.logger.Info("Querying IMDS for VM instance information")

	// Step 1: Query IMDS instance endpoint to get VM resource ID
	const imdsInstanceURL = "http://169.254.169.254/metadata/instance?api-version=2025-04-07"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsInstanceURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create IMDS request: %w", err)
	}

	req.Header.Set("Metadata", "true")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query IMDS instance endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("IMDS instance request failed with status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read IMDS response: %w", err)
	}

	// Parse the instance response to get resource ID
	var instanceInfo imdsInstanceInfo
	if err := json.Unmarshal(body, &instanceInfo); err != nil {
		return "", fmt.Errorf("failed to parse IMDS instance response: %w", err)
	}

	resourceID := instanceInfo.Compute.ResourceID
	if resourceID == "" {
		return "", fmt.Errorf("failed to get VM resource ID from IMDS instance endpoint")
	}

	// Step 2: Parse resource ID to extract subscription, resource group, and VM name
	_, resourceGroup, vmName, err := parseVMResourceID(resourceID)
	if err != nil {
		return "", fmt.Errorf("failed to parse VM resource ID: %w", err)
	}

	// Step 3: Get VM details from ARM (clients should already be set up by Execute)
	if i.vmClient == nil {
		return "", fmt.Errorf("VM client not initialized - ensure setUpClients was called")
	}

	vmResp, err := i.vmClient.Get(ctx, resourceGroup, vmName, nil)
	if err != nil {
		return "", fmt.Errorf("failed to get VM details from ARM: %w", err)
	}

	vm := vmResp.VirtualMachine
	if vm.Identity == nil {
		return "", fmt.Errorf("no managed identity found on this Azure VM")
	}

	// First, check for user-assigned managed identities
	if len(vm.Identity.UserAssignedIdentities) > 0 {
		for _, identity := range vm.Identity.UserAssignedIdentities {
			if identity != nil && identity.ClientID != nil && *identity.ClientID != "" {
				i.logger.Infof("Found user-assigned managed identity with client ID: %s", *identity.ClientID)
				return *identity.ClientID, nil
			}
		}
	}

	// Fall back to system-assigned managed identity
	if vm.Identity.PrincipalID != nil && *vm.Identity.PrincipalID != "" {
		// For system-assigned identity, we need to get the client ID from the principal ID
		// The principal ID is the object ID; we'll use that for RBAC assignment
		i.logger.Infof("Found system-assigned managed identity with principal ID: %s", *vm.Identity.PrincipalID)
		return *vm.Identity.PrincipalID, nil
	}

	return "", fmt.Errorf("no managed identity found on this Azure VM")
}

func (i *AzureVmInstaller) validateManagedCluster(ctx context.Context) error {
	i.logger.Info("Validating target AKS Managed Cluster requirements for Azure RBAC authentication")

	cluster, err := i.getAKSCluster(ctx)
	if err != nil {
		return fmt.Errorf("failed to get AKS cluster info: %w", err)
	}

	// Check if Azure RBAC is enabled
	if cluster.Properties == nil ||
		cluster.Properties.AADProfile == nil ||
		cluster.Properties.AADProfile.EnableAzureRBAC == nil ||
		!*cluster.Properties.AADProfile.EnableAzureRBAC {
		return fmt.Errorf("target AKS cluster '%s' must have Azure RBAC enabled for node authentication", to.String(cluster.Name))
	}

	i.logger.Infof("Target AKS cluster '%s' has Azure RBAC enabled", to.String(cluster.Name))
	return nil
}

// assignRBACRoles assigns required RBAC roles to the Azure VM's managed identity
func (i *AzureVmInstaller) assignRBACRoles(ctx context.Context, managedIdentityID string) error {
	if managedIdentityID == "" {
		return fmt.Errorf("managed identity ID is empty")
	}

	// Track assignment results
	requiredRoles := i.getRoleAssignments()
	var assignmentErrors []error
	for idx, role := range requiredRoles {
		i.logger.Infof("📋 [%d/%d] Assigning role '%s' on scope: %s", idx+1, len(requiredRoles), role.roleName, role.scope)

		if err := i.assignRole(ctx, managedIdentityID, role.roleID, role.scope, role.roleName); err != nil {
			i.logger.Errorf("❌ Failed to assign role '%s': %v", role.roleName, err)
			assignmentErrors = append(assignmentErrors, fmt.Errorf("role '%s': %w", role.roleName, err))
		} else {
			i.logger.Infof("✅ Successfully assigned role '%s'", role.roleName)
		}
	}

	if len(assignmentErrors) > 0 {
		i.logger.Errorf("⚠️  RBAC role assignment completed with %d failures", len(assignmentErrors))
		for _, err := range assignmentErrors {
			i.logger.Errorf("   - %v", err)
		}
		return fmt.Errorf("failed to assign %d out of %d RBAC roles", len(assignmentErrors), len(requiredRoles))
	}

	// wait for permissions to propagate
	i.logger.Infof("⏳ Starting permission polling for managed identity with ID: %s (this may take a few minutes)...", managedIdentityID)
	if err := i.waitForPermissions(ctx, managedIdentityID); err != nil {
		i.logger.Errorf("Failed while waiting for RBAC permissions: %v", err)
		return fmt.Errorf("azure vm bootstrap setup failed while waiting for RBAC permissions: %w", err)
	}

	i.logger.Info("🎉 All RBAC roles assigned successfully!")
	return nil
}

// assignRole creates a role assignment for the given principal, role, and scope
// Implements retry logic with exponential backoff to handle Azure AD replication delays
func (i *AzureVmInstaller) assignRole(
	ctx context.Context, principalID, roleDefinitionID, scope, roleName string,
) error {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		i.config.Azure.SubscriptionID, roleDefinitionID)

	const (
		maxRetries   = 5
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			delay := min(initialDelay*time.Duration(1<<(attempt-1)), maxDelay)
			i.logger.Infof("⏳ Retrying role assignment after %v (attempt %d/%d)...", delay, attempt+1, maxRetries)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		roleAssignmentName := uuid.New().String()
		i.logger.Debugf("Calling Azure API to create role assignment with ID: %s (attempt %d/%d)", roleAssignmentName, attempt+1, maxRetries)

		// Set PrincipalType to ServicePrincipal for managed identities
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
		if _, err := i.roleAssignmentsClient.Create(ctx, scope, roleAssignmentName, assignment, nil); err != nil {
			lastErr = err
			errStr := err.Error()

			// Check for common error patterns
			if strings.Contains(errStr, "403") || strings.Contains(errStr, "Forbidden") {
				return fmt.Errorf("insufficient permissions to assign roles - ensure the user/service principal has Owner or User Access Administrator role on the target cluster: %w", err)
			}
			if strings.Contains(errStr, "RoleAssignmentExists") {
				i.logger.Info("ℹ️  Role assignment already exists (detected from error)")
				return nil
			}

			// PrincipalNotFound is retriable - likely Azure AD replication delay
			if strings.Contains(errStr, "PrincipalNotFound") {
				i.logger.Warnf("⚠️  Principal not found (Azure AD replication delay) - will retry...")
				// Provide detailed error information on last attempt only
				if attempt == maxRetries-1 {
					i.logger.Errorf("❌ Role assignment creation failed after %d attempts:", maxRetries)
					i.logger.Errorf("   Principal ID: %s", principalID)
					i.logger.Errorf("   Role Name: %s", roleName)
					i.logger.Errorf("   Role Definition ID: %s", fullRoleDefinitionID)
					i.logger.Errorf("   Scope: %s", scope)
					i.logger.Errorf("   Assignment Name: %s", roleAssignmentName)
					i.logger.Errorf("   Azure API Error: %v", err)
				}
				continue // Retry
			}

			// Non-retriable error - log details and return
			i.logger.Errorf("❌ Role assignment creation failed:")
			i.logger.Errorf("   Principal ID: %s", principalID)
			i.logger.Errorf("   Role Name: %s", roleName)
			i.logger.Errorf("   Role Definition ID: %s", fullRoleDefinitionID)
			i.logger.Errorf("   Scope: %s", scope)
			i.logger.Errorf("   Assignment Name: %s", roleAssignmentName)
			i.logger.Errorf("   Azure API Error: %v", err)
			return fmt.Errorf("failed to create role assignment: %s", err)
		}

		// Success
		i.logger.Debugf("✅ Role assignment created successfully")
		return nil
	}

	// Max retries exhausted
	return fmt.Errorf("failed to assign role after %d attempts due to Azure AD replication delay - managed identity not found: %w", maxRetries, lastErr)
}

// waitForPermissions waits for RBAC permissions propagation with timeout
func (i *AzureVmInstaller) waitForPermissions(ctx context.Context, managedIdentityID string) error {
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
			if hasPermissions, err := i.checkRequiredPermissions(ctx, managedIdentityID); err == nil && hasPermissions {
				i.logger.Info("✅ All required RBAC permissions are now available!")
				return nil
			} else if err != nil {
				i.logger.Warnf("Error while checking permissions: %s", err)
			}
			i.logger.Info("⏳ Some permissions are still missing, will check again in 10 seconds...")
		}
	}
}
