package arc

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// RoleAssignment represents a role assignment configuration
type RoleAssignment struct {
	RoleName string
	Scope    string
	RoleID   string
}

// Base provides common functionality for both Installer and Uninstaller
type Base struct {
	config       *config.Config
	logger       *logrus.Logger
	authProvider *auth.AuthProvider
}

// NewBase creates a new Arc base instance
func NewBase(logger *logrus.Logger) *Base {
	return &Base{
		config:       config.GetConfig(),
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
	}
}

// GetArcMachine retrieves Arc machine using Azure SDK
func (ab *Base) GetArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	arcMachineName := ab.config.GetArcMachineName()
	arcResourceGroup := ab.config.GetArcResourceGroup()

	cred, err := ab.authProvider.UserCredential(ctx, ab.config)
	if err != nil {
		return nil, fmt.Errorf("failed to get authentication credential: %w", err)
	}
	ab.logger.Debug("Using user credential for machine info lookup")

	// Create hybrid compute machines client
	client, err := armhybridcompute.NewMachinesClient(ab.config.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create hybrid compute client: %w", err)
	}

	// Get machine info using Azure SDK
	ab.logger.Infof("Getting Arc machine info for: %s in resource group: %s", arcMachineName, arcResourceGroup)
	result, err := client.Get(ctx, arcResourceGroup, arcMachineName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get Arc machine info via SDK: %w", err)
	}
	machine := result.Machine
	ab.logger.Infof("Successfully retrieved Arc machine info: %s (ID: %s)", to.String(machine.Name), to.String(machine.ID))
	return &result.Machine, nil
}

// checkRequiredPermissions verifies if the Arc managed identity has all required permissions by querying role assignments using user credentials
func (ab *Base) checkRequiredPermissions(ctx context.Context, principalID string) (bool, error) {
	// Get Arc machine info to get the Arc machine resource ID
	arcMachine, err := ab.GetArcMachine(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get Arc machine info for permission checking: %w", err)
	}

	// Use the user's credential to query role assignments
	cred, err := ab.authProvider.UserCredential(ctx, ab.config)
	if err != nil {
		return false, fmt.Errorf("failed to get user credential: %w", err)
	}

	// Create role assignments client
	client, err := armauthorization.NewRoleAssignmentsClient(ab.config.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create role assignments client: %w", err)
	}

	// Check each required role assignment
	requiredRoles := ab.getRoleAssignments(arcMachine)
	for _, required := range requiredRoles {
		hasRole, err := ab.checkRoleAssignment(ctx, client, principalID, required.RoleID, required.Scope)
		if err != nil {
			return false, fmt.Errorf("error checking role %s on scope %s: %w", required.RoleName, required.Scope, err)
		}
		if !hasRole {
			ab.logger.Infof("❌ Missing role assignment: %s on %s", required.RoleName, required.Scope)
			return false, nil
		}
		ab.logger.Infof("✅ Found role assignment: %s on %s", required.RoleName, required.Scope)
	}

	return true, nil
}

func (ab *Base) getRoleAssignments(arcMachine *armhybridcompute.Machine) []RoleAssignment {
	return []RoleAssignment{
		{"Reader (Target Cluster)", ab.config.Azure.TargetCluster.ResourceID, roleDefinitionIDs["Reader"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", ab.config.Azure.TargetCluster.ResourceID, roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", ab.config.Azure.TargetCluster.ResourceID, roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
}

// checkRoleAssignment checks if a principal has a specific role assignment on a scope
func (ab *Base) checkRoleAssignment(ctx context.Context, client *armauthorization.RoleAssignmentsClient, principalID, roleDefinitionID, scope string) (bool, error) {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		ab.config.Azure.SubscriptionID, roleDefinitionID)

	// List role assignments for the scope
	pager := client.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
		Filter: nil, // We'll filter programmatically
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("failed to list role assignments for scope %s: %w", scope, err)
		}

		// Check each role assignment
		for _, assignment := range page.Value {
			if assignment.Properties != nil &&
				assignment.Properties.PrincipalID != nil &&
				assignment.Properties.RoleDefinitionID != nil &&
				*assignment.Properties.PrincipalID == principalID &&
				*assignment.Properties.RoleDefinitionID == fullRoleDefinitionID {
				return true, nil
			}
		}
	}

	return false, nil
}

// CreateHybridComputeClient creates an authenticated Azure hybrid compute client
func (ab *Base) CreateHybridComputeClient(ctx context.Context) (*armhybridcompute.MachinesClient, error) {
	cred, err := ab.authProvider.UserCredential(ctx, ab.config)
	if err != nil {
		return nil, fmt.Errorf("failed to get authentication credential: %w", err)
	}

	client, err := armhybridcompute.NewMachinesClient(ab.config.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create hybrid compute client: %w", err)
	}

	return client, nil
}

// CreateRoleAssignmentsClient creates an Azure role assignments client with proper authentication
func (ab *Base) CreateRoleAssignmentsClient(ctx context.Context) (*armauthorization.RoleAssignmentsClient, error) {
	cred, err := ab.authProvider.UserCredential(ctx, ab.config)
	if err != nil {
		return nil, fmt.Errorf("failed to get credentials for role assignment: %w", err)
	}

	client, err := armauthorization.NewRoleAssignmentsClient(ab.config.Azure.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create role assignments client: %w", err)
	}

	return client, nil
}
