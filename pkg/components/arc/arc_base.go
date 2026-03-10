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

func (ab *base) getRoleAssignments() []roleAssignment {
	return []roleAssignment{
		{"Reader (Target Cluster)", ab.config.GetTargetClusterID(), roleDefinitionIDs["Reader"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", ab.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", ab.config.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
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
