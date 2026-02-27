package arc

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// RoleAssignment represents a role assignment configuration
type roleAssignment struct {
	roleName string
	scope    string
	roleID   string
}

// base provides common functionality that's common for both Installer and Uninstaller
type base struct {
	config                     *config.Config
	logger                     *logrus.Logger
	authProvider               *auth.AuthProvider
	hybridComputeMachineClient *armhybridcompute.MachinesClient
	mcClient                   *armcontainerservice.ManagedClustersClient
	roleAssignmentsClient      roleAssignmentsClient
}

// newbase creates a new Arc base instance which will be shared by Installer and Uninstaller
func newBase(logger *logrus.Logger) *base {
	return &base{
		config: config.GetConfig(),
		logger: logger,
	}
}

func (ab *base) setUpClients(ctx context.Context) error {
	// Ensure user authentication(SP or CLI) is set up
	if err := ab.ensureAuthentication(ctx); err != nil {
		return fmt.Errorf("fail to ensureAuthentication: %w", err)
	}

	cred, err := auth.NewAuthProvider().UserCredential(config.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to get authentication credential: %w", err)
	}

	// Create hybrid compute machines client
	hybridComputeMachineClient, err := armhybridcompute.NewMachinesClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create hybrid compute client: %w", err)
	}

	// Create managed clusters client
	mcClient, err := armcontainerservice.NewManagedClustersClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create managed clusters client: %w", err)
	}

	// Create role assignments client
	azureClient, err := armauthorization.NewRoleAssignmentsClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create role assignments client: %w", err)
	}

	ab.hybridComputeMachineClient = hybridComputeMachineClient
	ab.mcClient = mcClient
	ab.roleAssignmentsClient = &azureRoleAssignmentsClient{client: azureClient}
	return nil
}

// getArcMachine retrieves Arc machine using Azure SDK
func (ab *base) getArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	arcMachineName := ab.config.GetArcMachineName()
	arcResourceGroup := ab.config.GetArcResourceGroup()

	ab.logger.Infof("Getting Arc machine info for: %s in resource group: %s", arcMachineName, arcResourceGroup)
	result, err := ab.hybridComputeMachineClient.Get(ctx, arcResourceGroup, arcMachineName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get Arc machine info via SDK: %w", err)
	}
	machine := result.Machine
	ab.logger.Infof("Successfully retrieved Arc machine info: %s (ID: %s)", to.String(machine.Name), to.String(machine.ID))
	return &result.Machine, nil
}

func (ab *base) getAKSCluster(ctx context.Context) (*armcontainerservice.ManagedCluster, error) {
	clusterName := ab.config.GetTargetClusterName()
	clusterResourceGroup := ab.config.GetTargetClusterResourceGroup()

	ab.logger.Infof("Getting AKS cluster info for: %s in resource group: %s", clusterName, clusterResourceGroup)
	result, err := ab.mcClient.Get(ctx, clusterResourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS cluster info via SDK: %w", err)
	}
	cluster := result.ManagedCluster
	ab.logger.Infof("Successfully retrieved AKS cluster info: %s (ID: %s)", to.String(cluster.Name), to.String(cluster.ID))
	return &result.ManagedCluster, nil
}

// checkRequiredPermissions verifies if the Arc managed identity has all required permissions by querying role assignments using user credentials
func (ab *base) checkRequiredPermissions(ctx context.Context, principalID string) (bool, error) {
	// Check each required role assignment
	requiredRoles := ab.getRoleAssignments()
	for _, required := range requiredRoles {
		hasRole, err := ab.checkRoleAssignment(ctx, principalID, required.roleID, required.scope)
		if err != nil {
			return false, fmt.Errorf("error checking role %s on scope %s: %w", required.roleName, required.scope, err)
		}
		if !hasRole {
			ab.logger.Infof("❌ Missing role assignment: %s on %s", required.roleName, required.scope)
			return false, nil
		}
		ab.logger.Infof("✅ Found role assignment: %s on %s", required.roleName, required.scope)
	}

	return true, nil
}

func (ab *base) getRoleAssignments() []roleAssignment {
	return []roleAssignment{
		{"Reader (Target Cluster)", ab.config.GetTargetClusterID(), roleDefinitionIDs["Reader"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", ab.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", ab.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
}

// checkRoleAssignment checks if a principal has a specific role assignment on a scope
func (ab *base) checkRoleAssignment(ctx context.Context, principalID, roleDefinitionID, scope string) (bool, error) {
	// Build the full role definition ID
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		ab.config.Azure.SubscriptionID, roleDefinitionID)

	// List role assignments for the scope
	pager := ab.roleAssignmentsClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
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

// ensureAuthentication ensures the appropriate authentication (SP or CLI) method is set up
func (ab *base) ensureAuthentication(ctx context.Context) error {
	if ab.config.IsSPConfigured() {
		ab.logger.Info("🔐 Using service principal authentication")
		return nil
	}

	ab.logger.Info("🔐 Checking Azure CLI authentication status...")
	tenantID := ab.config.GetTenantID()
	if err := ab.authProvider.EnsureAuthenticated(ctx, tenantID); err != nil {
		ab.logger.Errorf("Failed to ensure Azure CLI authentication: %v", err)
		return err
	}
	ab.logger.Info("✅ Azure CLI authentication verified")
	return nil
}
