package arc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// uninstallArcTask implements phases.Task for Arc cleanup during unbootstrap.
type uninstallArcTask struct {
	cfg    *config.Config
	logger *slog.Logger

	// lazily initialised by setUpClients
	hybridComputeClient   *armhybridcompute.MachinesClient
	roleAssignmentsClient roleAssignmentsClient
}

// UninstallArc returns a phases.Task that unregisters the machine from Azure
// Arc, removes RBAC role assignments, disconnects the agent, and removes all
// Arc binaries and configuration. It is a no-op when Arc is disabled.
//
// The task uses best-effort semantics: individual cleanup steps log warnings
// on failure but do not abort the overall task, so that as much cleanup as
// possible is performed.
func UninstallArc(logger *slog.Logger) phases.Task {
	return &uninstallArcTask{
		cfg:    config.GetConfig(),
		logger: logger,
	}
}

func (t *uninstallArcTask) Name() string { return "uninstall-arc" }

func (t *uninstallArcTask) Do(ctx context.Context) error {
	if t.cfg == nil {
		t.logger.Info("configuration not loaded, skipping Arc uninstall")
		return nil
	}
	if !t.cfg.IsARCEnabled() {
		t.logger.Info("Azure Arc is disabled, skipping uninstall")
		return nil
	}

	t.logger.Info("starting Arc cleanup")

	// Step 1: Set up Azure SDK clients.
	if err := t.setUpClients(ctx); err != nil {
		return fmt.Errorf("arc cleanup setup failed at client setup: %w", err)
	}

	arcMachine, err := t.getArcMachine(ctx)
	if err != nil {
		t.logger.Warn("failed to get Arc machine (continuing cleanup)", "error", err)
	}

	var failedOps []string

	// Step 2: Remove RBAC role assignments (while authentication still works).
	if err := t.removeRBACRoles(ctx, arcMachine); err != nil {
		t.logger.Warn("failed to remove RBAC roles (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "RBAC role removal")
	}

	// Step 3: Delete Arc machine resource from Azure.
	if err := t.unregisterArcMachine(ctx); err != nil {
		t.logger.Warn("failed to unregister Arc machine (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "Arc machine unregistration")
	}

	// Step 4: Local disconnect (removes agent state).
	if err := t.disconnectArcMachine(ctx); err != nil {
		t.logger.Warn("failed to disconnect Arc machine (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "Arc machine disconnection")
	}

	// Step 5: Remove agent binaries and configuration.
	if err := t.removeArcAgentBinary(ctx); err != nil {
		t.logger.Warn("failed to remove Arc agent binaries (continuing cleanup)", "error", err)
		failedOps = append(failedOps, "Arc agent binary removal")
	}

	if len(failedOps) > 0 {
		t.logger.Warn("Arc cleanup completed with failures",
			"failedCount", len(failedOps),
			"failedOps", strings.Join(failedOps, ", "))
		// Best-effort: don't return error so unbootstrap can continue.
		return nil
	}

	t.logger.Info("Arc cleanup completed successfully")
	return nil
}

// --- client setup ---

func (t *uninstallArcTask) setUpClients(ctx context.Context) error {
	if err := t.ensureAuthentication(ctx); err != nil {
		return fmt.Errorf("ensure authentication: %w", err)
	}

	cred, err := auth.NewAuthProvider().UserCredential(t.cfg)
	if err != nil {
		return fmt.Errorf("get authentication credential: %w", err)
	}

	subID := t.cfg.GetSubscriptionID()

	hc, err := armhybridcompute.NewMachinesClient(subID, cred, nil)
	if err != nil {
		return fmt.Errorf("create hybrid compute client: %w", err)
	}

	ra, err := armauthorization.NewRoleAssignmentsClient(subID, cred, nil)
	if err != nil {
		return fmt.Errorf("create role assignments client: %w", err)
	}

	t.hybridComputeClient = hc
	t.roleAssignmentsClient = &azureRoleAssignmentsClient{client: ra}
	return nil
}

func (t *uninstallArcTask) ensureAuthentication(ctx context.Context) error {
	if t.cfg.IsSPConfigured() {
		t.logger.Info("using service principal authentication")
		return nil
	}

	t.logger.Info("checking Azure CLI authentication status")
	if err := auth.NewAuthProvider().EnsureAuthenticated(ctx, t.cfg.GetTenantID()); err != nil {
		return fmt.Errorf("ensure Azure CLI authentication: %w", err)
	}
	t.logger.Info("Azure CLI authentication verified")
	return nil
}

// --- Arc machine operations ---

func (t *uninstallArcTask) getArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	name := t.cfg.GetArcMachineName()
	rg := t.cfg.GetArcResourceGroup()

	t.logger.Info("getting Arc machine info", "name", name, "resourceGroup", rg)
	result, err := t.hybridComputeClient.Get(ctx, rg, name, nil)
	if err != nil {
		return nil, fmt.Errorf("get Arc machine info: %w", err)
	}
	return &result.Machine, nil
}

func (t *uninstallArcTask) unregisterArcMachine(ctx context.Context) error {
	name := t.cfg.GetArcMachineName()
	rg := t.cfg.GetArcResourceGroup()
	t.logger.Info("deleting Arc machine resource", "name", name, "resourceGroup", rg)

	if _, err := t.hybridComputeClient.Delete(ctx, rg, name, nil); err != nil {
		if strings.Contains(err.Error(), "ResourceNotFound") || strings.Contains(err.Error(), "NotFound") {
			t.logger.Info("Arc machine resource not found (already deleted)")
			return nil
		}
		return fmt.Errorf("delete Arc machine resource: %w", err)
	}

	t.logger.Info("Arc machine unregistered from Azure")
	return nil
}

func (t *uninstallArcTask) disconnectArcMachine(ctx context.Context) error {
	if err := utilexec.RunCmd(ctx, t.logger, utilexec.Azcmagent(), "disconnect", "--force-local-only"); err != nil {
		return fmt.Errorf("disconnect Arc machine: %w", err)
	}
	t.logger.Info("Arc machine disconnected")
	return nil
}

// --- RBAC ---

// TODO(security): These roles are over-privileged — see components/arc/v20260301/arc_rbac.go for details.
// Will be restricted to least-privilege once the node auth design is confirmed.
func (t *uninstallArcTask) getRoleAssignments() []roleAssignment {
	return []roleAssignment{
		{"Reader (Target Cluster)", t.cfg.GetTargetClusterID(), roleDefinitionIDs["Reader"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", t.cfg.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", t.cfg.GetTargetClusterID(), roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
}

func (t *uninstallArcTask) removeRBACRoles(ctx context.Context, arcMachine *armhybridcompute.Machine) error {
	managedIdentityID := getArcMachineIdentityID(arcMachine)
	if managedIdentityID == "" {
		t.logger.Info("no managed identity found for Arc machine")
		return nil
	}

	t.logger.Info("removing role assignments", "principalID", managedIdentityID)

	var removalErrors []string
	for _, role := range t.getRoleAssignments() {
		if err := t.removeRoleAssignment(ctx, managedIdentityID, role.roleID, role.scope, role.roleName); err != nil {
			t.logger.Warn("failed to remove role assignment", "role", role.roleName, "scope", role.scope, "error", err)
			removalErrors = append(removalErrors, fmt.Sprintf("%s: %v", role.roleName, err))
		}
	}

	if len(removalErrors) > 0 {
		return fmt.Errorf("failed to remove some role assignments: %s", strings.Join(removalErrors, "; "))
	}

	t.logger.Info("all RBAC role assignments removed")
	return nil
}

func (t *uninstallArcTask) removeRoleAssignment(ctx context.Context, principalID, roleDefinitionID, scope, roleName string) error {
	fullRoleDefinitionID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		t.cfg.Azure.SubscriptionID, roleDefinitionID)

	pager := t.roleAssignmentsClient.NewListForScopePager(scope, &armauthorization.RoleAssignmentsClientListForScopeOptions{})

	var assignmentsToDelete []string
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list role assignments for scope %s: %w", scope, err)
		}

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

	for _, name := range assignmentsToDelete {
		if _, err := t.roleAssignmentsClient.Delete(ctx, scope, name, nil); err != nil {
			if strings.Contains(err.Error(), "RoleAssignmentNotFound") || strings.Contains(err.Error(), "NotFound") {
				continue
			}
			return fmt.Errorf("delete role assignment %s: %w", name, err)
		}
	}

	if len(assignmentsToDelete) == 0 {
		t.logger.Debug("no role assignments found", "role", roleName, "scope", scope)
	}

	return nil
}

// --- local cleanup ---

func (t *uninstallArcTask) removeArcAgentBinary(ctx context.Context) error {
	if !isArcAgentInstalled() {
		t.logger.Info("Azure Arc agent not found, already removed or never installed")
		return nil
	}

	// Ensure disconnection happened.
	if err := utilexec.RunCmd(ctx, t.logger, utilexec.Azcmagent(), "disconnect", "--force-local-only"); err != nil {
		t.logger.Debug("Arc disconnect command failed or already disconnected")
	}

	// Stop Arc agent services.
	for _, service := range arcServices {
		if utilexec.ServiceExists(ctx, t.logger, service) {
			if err := utilexec.StopService(ctx, t.logger, service); err != nil {
				t.logger.Debug("failed to stop service", "service", service, "error", err)
			}
			if err := utilexec.DisableService(ctx, t.logger, service); err != nil {
				t.logger.Debug("failed to disable service", "service", service, "error", err)
			}
		}
	}

	// Remove binaries.
	for _, path := range arcBinaryPaths {
		if err := utilexec.RemoveFileIfExists(path); err != nil {
			t.logger.Debug("failed to remove binary", "path", path, "error", err)
		}
	}

	// Remove directories.
	for _, dir := range arcDirectories {
		if err := utilexec.RemoveAllIfExists(dir); err != nil {
			t.logger.Debug("failed to remove directory", "dir", dir, "error", err)
		}
	}

	// Remove systemd service files.
	for _, file := range arcServiceFiles {
		if err := utilexec.RemoveFileIfExists(file); err != nil {
			t.logger.Debug("failed to remove service file", "file", file, "error", err)
		}
	}

	if err := utilexec.ReloadSystemd(ctx, t.logger); err != nil {
		t.logger.Debug("failed to reload systemd daemon")
	}

	if isArcAgentInstalled() {
		return fmt.Errorf("arc agent binary still present after cleanup")
	}

	t.logger.Info("Azure Arc agent binaries and configuration removed")
	return nil
}
