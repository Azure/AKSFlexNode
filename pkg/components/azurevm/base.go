package azurevm

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v5"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/auth"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// roleAssignment represents a role assignment configuration
type roleAssignment struct {
	roleName string
	scope    string
	roleID   string
}

// roleDefinitionIDs maps role names to their Azure role definition IDs
var roleDefinitionIDs = map[string]string{
	"Reader": "acdd72a7-3385-48ef-bd42-f606fba81ae7",
	"Azure Kubernetes Service RBAC Cluster Admin": "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b",
	"Azure Kubernetes Service Cluster Admin Role": "0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8",
}

// roleAssignmentsClient defines the interface for role assignment operations
type roleAssignmentsClient interface {
	Create(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error)
	Delete(ctx context.Context, scope string, roleAssignmentName string, options *armauthorization.RoleAssignmentsClientDeleteOptions) (armauthorization.RoleAssignmentsClientDeleteResponse, error)
	NewListForScopePager(scope string, options *armauthorization.RoleAssignmentsClientListForScopeOptions) *runtime.Pager[armauthorization.RoleAssignmentsClientListForScopeResponse]
}

// azureRoleAssignmentsClient wraps the real Azure SDK client to implement our interface
type azureRoleAssignmentsClient struct {
	client *armauthorization.RoleAssignmentsClient
}

func (a *azureRoleAssignmentsClient) Create(ctx context.Context, scope string, roleAssignmentName string, parameters armauthorization.RoleAssignmentCreateParameters, options *armauthorization.RoleAssignmentsClientCreateOptions) (armauthorization.RoleAssignmentsClientCreateResponse, error) {
	return a.client.Create(ctx, scope, roleAssignmentName, parameters, options)
}

func (a *azureRoleAssignmentsClient) Delete(ctx context.Context, scope string, roleAssignmentName string, options *armauthorization.RoleAssignmentsClientDeleteOptions) (armauthorization.RoleAssignmentsClientDeleteResponse, error) {
	return a.client.Delete(ctx, scope, roleAssignmentName, options)
}

func (a *azureRoleAssignmentsClient) NewListForScopePager(scope string, options *armauthorization.RoleAssignmentsClientListForScopeOptions) *runtime.Pager[armauthorization.RoleAssignmentsClientListForScopeResponse] {
	return a.client.NewListForScopePager(scope, options)
}

// base provides common functionality for both Installer and UnInstaller
type base struct {
	config                *config.Config
	logger                *logrus.Logger
	authProvider          *auth.AuthProvider
	mcClient              *armcontainerservice.ManagedClustersClient
	vmClient              *armcompute.VirtualMachinesClient
	roleAssignmentsClient roleAssignmentsClient
}

// newBase creates a new base instance
func newBase(logger *logrus.Logger) *base {
	return &base{
		config:       config.GetConfig(),
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
	}
}

func (b *base) setUpClients(ctx context.Context) error {
	// Ensure user authentication (SP or CLI) is set up
	if err := b.ensureAuthentication(ctx); err != nil {
		return fmt.Errorf("fail to ensureAuthentication: %w", err)
	}

	cred, err := auth.NewAuthProvider().UserCredential(config.GetConfig())
	if err != nil {
		return fmt.Errorf("failed to get authentication credential: %w", err)
	}

	// Create managed clusters client
	mcClient, err := armcontainerservice.NewManagedClustersClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create managed clusters client: %w", err)
	}

	// Create VM client
	vmClient, err := armcompute.NewVirtualMachinesClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create virtual machines client: %w", err)
	}

	// Create role assignments client
	azureClient, err := armauthorization.NewRoleAssignmentsClient(config.GetConfig().GetSubscriptionID(), cred, nil)
	if err != nil {
		return fmt.Errorf("failed to create role assignments client: %w", err)
	}

	b.mcClient = mcClient
	b.vmClient = vmClient
	b.roleAssignmentsClient = &azureRoleAssignmentsClient{client: azureClient}
	return nil
}

func (b *base) getAKSCluster(ctx context.Context) (*armcontainerservice.ManagedCluster, error) {
	clusterName := b.config.GetTargetClusterName()
	clusterResourceGroup := b.config.GetTargetClusterResourceGroup()

	b.logger.Infof("Getting AKS cluster info for: %s in resource group: %s", clusterName, clusterResourceGroup)
	result, err := b.mcClient.Get(ctx, clusterResourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS cluster info via SDK: %w", err)
	}
	cluster := result.ManagedCluster
	b.logger.Infof("Successfully retrieved AKS cluster info: %s (ID: %s)", to.String(cluster.Name), to.String(cluster.ID))
	return &result.ManagedCluster, nil
}

// checkRequiredPermissions verifies if the managed identity has all required permissions
func (b *base) checkRequiredPermissions(ctx context.Context, principalID string) (bool, error) {
	requiredRoles := b.getRoleAssignments()
	for _, required := range requiredRoles {
		hasRole, err := b.checkRoleAssignment(ctx, principalID, required.roleID, required.scope)
		if err != nil {
			return false, fmt.Errorf("error checking role %s on scope %s: %w", required.roleName, required.scope, err)
		}
		if !hasRole {
			b.logger.Infof("❌ Missing role assignment: %s on %s", required.roleName, required.scope)
			return false, nil
		}
		b.logger.Infof("✅ Found role assignment: %s on %s", required.roleName, required.scope)
	}

	return true, nil
}

func (b *base) getRoleAssignments() []roleAssignment {
	return []roleAssignment{
		{"Reader (Target Cluster)", b.config.GetTargetClusterID(), roleDefinitionIDs["Reader"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", b.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", b.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
}

// checkRoleAssignment checks if a principal has a specific role assignment on a scope
func (b *base) checkRoleAssignment(ctx context.Context, principalID, roleDefinitionID, scope string) (bool, error) {
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		b.config.Azure.SubscriptionID, roleDefinitionID)

	pager := b.roleAssignmentsClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{
		Filter: nil,
	})

	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return false, fmt.Errorf("failed to list role assignments for scope %s: %w", scope, err)
		}

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
func (b *base) ensureAuthentication(ctx context.Context) error {
	if b.config.IsSPConfigured() {
		b.logger.Info("🔐 Using service principal authentication")
		return nil
	}

	b.logger.Info("🔐 Checking Azure CLI authentication status...")
	tenantID := b.config.GetTenantID()
	if err := b.authProvider.EnsureAuthenticated(ctx, tenantID); err != nil {
		b.logger.Errorf("Failed to ensure Azure CLI authentication: %v", err)
		return err
	}
	b.logger.Info("✅ Azure CLI authentication verified")
	return nil
}
