package arc

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/utils"
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
	if !i.config.IsARCEnabled() {
		i.logger.Info("Azure Arc installation is disabled in configuration")
		return nil
	}
	// Ensure SP or CLI auth is ready for Arc agent setup
	if err := i.ensureAuthentication(ctx); err != nil {
		i.logger.Errorf("Authentication setup failed: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at authentication: %w", err)
	}
	// Ensure Arc agent is installed
	if !isArcAgentInstalled() {
		i.logger.Info("Azure Arc agent not found, installing...")
		if err := i.installArcAgentBinary(ctx); err != nil {
			return fmt.Errorf("failed to install Azure Arc agent binary: %w", err)
		}
	}
	return nil
}

// installArcAgentBinary downloads and installs the Azure Arc agent binary
func (i *Installer) installArcAgentBinary(ctx context.Context) error {
	i.logger.Info("Installing Azure Arc agent binary...")

	// Clean up any existing package state to avoid conflicts
	if err := utils.RunSystemCommand("dpkg", "--purge", "azcmagent"); err != nil {
		i.logger.Debug("No existing azcmagent package to remove")
	}

	// Create temporary directory for installation script
	tempDir, err := os.MkdirTemp("", "arc-install-*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tempDir); rmErr != nil {
			i.logger.Debug("Failed to clean up temp directory", "dir", tempDir, "error", rmErr)
		}
	}()

	// Download Arc agent installation script
	installScriptPath := filepath.Join(tempDir, "install_linux_azcmagent.sh")
	i.logger.Info("Downloading Azure Arc agent installation script...")

	if err := i.downloadArcInstallScript(ctx, installScriptPath); err != nil {
		return fmt.Errorf("failed to download Arc installation script: %w", err)
	}

	// Make script executable
	if err := os.Chmod(installScriptPath, 0o755); err != nil {
		return fmt.Errorf("failed to make installation script executable: %w", err)
	}

	// Execute installation script
	i.logger.Info("Running Azure Arc agent installation script...")
	if err := utils.RunSystemCommand("bash", installScriptPath); err != nil {
		return fmt.Errorf("failed to install Azure Arc agent: %w", err)
	}

	// Setup Arc-specific permissions (add service user to himds group)
	if err := i.setupArcPermissions(); err != nil {
		i.logger.Warnf("Failed to setup Arc permissions: %v", err)
		// Don't fail the installation for permission issues
	}

	i.logger.Info("Azure Arc agent binary installed successfully")
	return nil
}

// setupArcPermissions adds the aks-flex-node service user to the himds group
func (i *Installer) setupArcPermissions() error {
	i.logger.Info("Setting up Arc-specific permissions...")

	// Add service user to himds group (created by Arc agent installation)
	serviceUser := "aks-flex-node"
	if err := utils.RunSystemCommand("usermod", "-a", "-G", "himds", serviceUser); err != nil {
		return fmt.Errorf("failed to add %s to himds group: %w", serviceUser, err)
	}

	i.logger.Info("Successfully added service user to himds group")
	return nil
}

// downloadArcInstallScript downloads the Arc installation script using curl or wget
func (i *Installer) downloadArcInstallScript(ctx context.Context, destPath string) error {
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

	// Step 3: Validate managed cluster requirements
	i.logger.Info("Step 3: Validating Managed Cluster requirements")
	if err := i.validateManagedCluster(ctx); err != nil {
		i.logger.Errorf("Managed Cluster validation failed: %v", err)
		return fmt.Errorf("arc bootstrap setup failed at managed cluster validation: %w", err)
	}

	// Step 4: Assign RBAC roles to managed identity
	time.Sleep(10 * time.Second) // brief pause to ensure identity is ready
	i.logger.Info("Step 4: Assigning RBAC roles to managed identity")
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
	if !i.config.IsARCEnabled() {
		i.logger.Info("Azure Arc installation is disabled in configuration, skipping execution")
		return true
	}
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

func (i *Installer) validateManagedCluster(ctx context.Context) error {
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

	// Execute azcmagent command securely (avoid logging access token)
	if err := i.runAzcmagentSecurely("azcmagent", args); err != nil {
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
// Implements retry logic with exponential backoff to handle Azure AD replication delays
func (i *Installer) assignRole(
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
	return fmt.Errorf("failed to assign role after %d attempts due to Azure AD replication delay - arc managed identity not found: %w", maxRetries, lastErr)
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

// runAzcmagentSecurely executes azcmagent command without logging sensitive arguments
func (i *Installer) runAzcmagentSecurely(name string, args []string) error {
	// Create command args without exposing the access token in logs
	maskedArgs := make([]string, len(args))
	copy(maskedArgs, args)

	// Find and mask the access token
	for j := 0; j < len(maskedArgs)-1; j++ {
		if maskedArgs[j] == "--access-token" {
			maskedArgs[j+1] = "***REDACTED***"
			break
		}
	}

	i.logger.Infof("Executing command: %s %v", name, maskedArgs)

	// Execute the actual command with real args but capture output to avoid logging
	_, err := utils.RunCommandWithOutput(name, args...)
	if err != nil {
		return err
	}

	return nil
}
