package arc

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
)

// roleAssignmentsClient defines the interface for role assignment operations
// This interface wraps the Azure SDK client to enable testing with mocks
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
