package arc

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

// Installer handles Azure Arc installation operations
type Installer struct {
	*base
}

// NewInstaller creates a new Arc installer
func NewInstaller(logger *logrus.Logger) *Installer {
	return &Installer{
		base: newBase(logger),
	}
}

// Validate validates prerequisites for Arc installation
func (i *Installer) Validate(ctx context.Context) error {
	// Ensure SP or CLI auth is ready for Arc agent setup
	if err := i.ensureAuthentication(ctx); err != nil {
		i.logger.Errorf("Authentication setup failed: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at authentication: %w", err)
	}
	// Ensure Arc agent is installed and running
	if !isArcAgentInstalled() {
		i.logger.Info("Azure Arc agent not found")
		return fmt.Errorf("azure Arc agent not found - please run the installation script first:\n" +
			"curl -fsSL https://raw.githubusercontent.com/Azure/AKSFlexNode/main/scripts/install.sh | bash")
	}
	return nil
}

// GetName returns the step name
func (i *Installer) GetName() string {
	return "ArcInstall"
}

// Execute performs Arc setup as part of the bootstrap process
// This method is designed to be called from bootstrap steps and handles all Arc-related setup
// It stops on the first error to prevent partial setups
func (i *Installer) Execute(ctx context.Context) error {
	i.logger.Info("Starting Arc setup for bootstrap process")

	// Step 1: Set up Azure SDK clients
	if err := i.setUpClients(ctx); err != nil {
		return fmt.Errorf("arc bootstrap setup failed at client setup: %w", err)
	}

	// Step 2: Register Arc machine with Azure
	i.logger.Info("Step 2: Registering Arc machine with Azure")
	arcMachine, err := i.registerArcMachine(ctx)
	if err != nil {
		i.logger.Errorf("Failed to register Arc machine: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at machine registration: %w", err)
	}
	i.logger.Info("Successfully registered Arc machine with Azure")

	// Step 3: Assign RBAC roles to managed identity
	time.Sleep(10 * time.Second) // brief pause to ensure identity is ready
	i.logger.Info("Step 3: Assigning RBAC roles to managed identity")
	if err := i.assignRBACRoles(ctx, arcMachine); err != nil {
		i.logger.Errorf("Failed to assign RBAC roles: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at RBAC role assignment: %w", err)
	}
	i.logger.Info("Successfully assigned RBAC roles")

	i.logger.Info("Arc setup for bootstrap completed successfully")
	return nil
}

// IsCompleted checks if Arc setup has been completed
// This can be used by bootstrap steps to verify completion status
// Uses the same reliable logic as status collector for consistency
func (i *Installer) IsCompleted(ctx context.Context) bool {
	i.logger.Debug("Checking Arc setup completion status")

	// Check if Arc services are running
	if !isArcServicesRunning() {
		i.logger.Debug("Arc services are not running")
		return false
	}

	// Use same approach as status collector - check azcmagent show with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "azcmagent", "show")
	output, err := cmd.Output()
	if err != nil {
		i.logger.Debugf("azcmagent show failed: %v - Arc not ready", err)
		return false
	}

	// Parse output to check if agent is connected (same logic as status collector)
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Agent Status") && strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				status := strings.TrimSpace(parts[1])
				isConnected := strings.ToLower(status) == "connected"
				if isConnected {
					i.logger.Debug("Arc setup appears to be completed - agent is connected")
				} else {
					i.logger.Debugf("Arc agent status is '%s' - not ready", status)
				}
				return isConnected
			}
		}
	}

	i.logger.Debug("Could not find Agent Status in azcmagent show output - Arc not ready")
	return false
}

// registerArcMachine registers the machine with Azure Arc using the Arc agent
func (i *Installer) registerArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	i.logger.Info("Registering machine with Azure Arc using Arc agent")

	// Check if already registered
	machine, err := i.getArcMachine(ctx)
	if err == nil && machine != nil {
		i.logger.Infof("Machine already registered as Arc machine: %s", to.String(machine.Name))
		return machine, nil
	}

	// Register using Arc agent command
	if err := i.runArcAgentConnect(ctx); err != nil {
		return nil, fmt.Errorf("failed to register Arc machine using agent: %w", err)
	}

	// make sure registration is complete before proceeding
	// otherwise role assignment may fail due to identity not found
	return i.waitForArcRegistration(ctx)
}

func (i *Installer) waitForArcRegistration(ctx context.Context) (*armhybridcompute.Machine, error) {
	const (
		maxRetries   = 10
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	for attempt := 0; attempt < maxRetries; attempt++ {
		machine, err := i.getArcMachine(ctx)
		if err == nil &&
			machine != nil &&
			machine.Identity != nil &&
			machine.Identity.PrincipalID != nil {
			return machine, nil // Success!
		}
		i.logger.Infof("Arc machine not yet registered (attempt %d/%d): %s", attempt+1, maxRetries, err)

		delay := min(initialDelay*time.Duration(1<<attempt), maxDelay)
		i.logger.Infof("Registration attempt %d/%d, waiting %v...", attempt+1, maxRetries, delay)

		select {
		case <-time.After(delay):
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("arc registration timed out after %d attempts", maxRetries)
}

// runArcAgentConnect connects the machine to Azure Arc using the Arc agent
func (i *Installer) runArcAgentConnect(ctx context.Context) error {
	i.logger.Info("Connecting machine to Azure Arc using azcmagent")

	// Get Arc configuration details
	arcLocation := i.config.GetArcLocation()
	arcMachineName := i.config.GetArcMachineName()
	arcResourceGroup := i.config.GetArcResourceGroup()
	subscriptionID := i.config.GetSubscriptionID()
	tenantID := i.config.GetTenantID()

	// Build azcmagent connect command
	args := []string{
		"connect",
		"--resource-group", arcResourceGroup,
		"--tenant-id", tenantID,
		"--location", arcLocation,
		"--subscription-id", subscriptionID,
		"--resource-name", arcMachineName,
	}

	// Add Arc tags if any
	tags := i.config.GetArcTags()
	tagArgs := []string{}
	for key, value := range tags {
		tagArgs = append(tagArgs, "--tags", fmt.Sprintf("%s=%s", key, value))
	}
	args = append(args, tagArgs...)

	// Add authentication parameters
	// For CLI authentication, we need to preserve the user's environment
	if err := i.addAuthenticationArgs(ctx, &args); err != nil {
		return fmt.Errorf("failed to configure authentication for Arc agent: %w", err)
	}

	if err := utils.RunSystemCommand("azcmagent", args...); err != nil {
		return fmt.Errorf("failed to connect to Azure Arc: %w", err)
	}

	i.logger.Infof("Arc agent connect completed")
	return nil
}

// assignRBACRoles assigns required RBAC roles to the Arc machine's managed identity
func (i *Installer) assignRBACRoles(ctx context.Context, arcMachine *armhybridcompute.Machine) error {
	managedIdentityID := getArcMachineIdentityID(arcMachine)
	if managedIdentityID == "" {
		return fmt.Errorf("managed identity ID not found on Arc machine")
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
	i.logger.Infof("⏳ Starting permission polling for arc identity with ID: %s (this may take a few minutes)...", managedIdentityID)
	if err := i.waitForPermissions(ctx, managedIdentityID); err != nil {
		i.logger.Errorf("Failed while waiting for RBAC permissions: %v", err)
		return fmt.Errorf("arc bootstrap setup failed while waiting for RBAC permissions: %w", err)
	}

	i.logger.Info("🎉 All RBAC roles assigned successfully!")
	return nil
}

// assignRole creates a role assignment for the given principal, role, and scope
func (i *Installer) assignRole(
	ctx context.Context, principalID, roleDefinitionID, scope, roleName string) error {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		i.config.Azure.SubscriptionID, roleDefinitionID)

	roleAssignmentName := uuid.New().String()
	i.logger.Debugf("Calling Azure API to create role assignment with ID: %s", roleAssignmentName)
	assignment := armauthorization.RoleAssignmentCreateParameters{
		Properties: &armauthorization.RoleAssignmentProperties{
			PrincipalID:      &principalID,
			RoleDefinitionID: &fullRoleDefinitionID,
		},
	}
	// this create operation is synchronous - we need to wait for the role propagation to take effect afterwards
	if _, err := i.roleAssignmentsClient.Create(ctx, scope, roleAssignmentName, assignment, nil); err != nil {
		// Provide more detailed error information
		i.logger.Errorf("❌ Role assignment creation failed:")
		i.logger.Errorf("   Principal ID: %s", principalID)
		i.logger.Errorf("   Role Name: %s", roleName)
		i.logger.Errorf("   Role Definition ID: %s", fullRoleDefinitionID)
		i.logger.Errorf("   Scope: %s", scope)
		i.logger.Errorf("   Assignment Name: %s", roleAssignmentName)
		i.logger.Errorf("   Azure API Error: %v", err)

		// Check for common error patterns
		errStr := err.Error()
		if strings.Contains(errStr, "403") || strings.Contains(errStr, "Forbidden") {
			return fmt.Errorf("insufficient permissions to assign roles - ensure the user/service principal has Owner or User Access Administrator role on the target cluster: %w", err)
		}
		if strings.Contains(errStr, "RoleAssignmentExists") {
			i.logger.Info("ℹ️  Role assignment already exists (detected from error)")
			return nil
		}
		if strings.Contains(errStr, "PrincipalNotFound") { // TODO(wenxuan): this is retriable, add retry logic later
			return fmt.Errorf("arc managed identity not found - ensure Arc machine is properly registered: %w", err)
		}
		return fmt.Errorf("failed to create role assignment: %s", err)
	}

	i.logger.Debugf("✅ Role assignment created successfully")
	return nil
}

// waitForPermissions waits for RBAC permissions propagation with timeout
func (i *Installer) waitForPermissions(ctx context.Context, managedIdentityID string) error {
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

// addAuthenticationArgs adds appropriate authentication parameters to the azcmagent command
func (i *Installer) addAuthenticationArgs(ctx context.Context, args *[]string) error {
	cred, err := i.authProvider.UserCredential(i.config)
	if err != nil {
		return fmt.Errorf("failed to get Azure credentials: %w", err)
	}

	accessToken, err := i.authProvider.GetAccessToken(ctx, cred)
	if err != nil {
		return fmt.Errorf("failed to get access token for Arc agent authentication: %w", err)
	}

	i.logger.Info("Using access token authentication for Arc agent")
	*args = append(*args, "--access-token", accessToken)
	return nil
}
