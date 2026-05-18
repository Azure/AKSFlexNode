package arc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/google/uuid"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type installArcTask struct {
	cfg          *config.Config
	logger       *slog.Logger
	authProvider *auth.AuthProvider

	// lazily initialised by setUpClients
	hybridComputeClient   *armhybridcompute.MachinesClient
	mcClient              *armcontainerservice.ManagedClustersClient
	roleAssignmentsClient *armauthorization.RoleAssignmentsClient
}

// InstallArc returns a task that registers the machine with Azure Arc, validates
// the target AKS cluster, and assigns RBAC roles to the Arc managed identity.
// It is a no-op when Arc is disabled in the config.
func InstallArc(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &installArcTask{
		cfg:          cfg,
		logger:       logger,
		authProvider: auth.NewAuthProvider(),
	}
}

func (t *installArcTask) Name() string { return "install-arc" }

func (t *installArcTask) Do(ctx context.Context) error {
	if !t.cfg.IsARCEnabled() {
		t.logger.Info("Arc is disabled, skipping installation")
		return nil
	}
	if err := copyAzureCLIAuth(); err != nil {
		return fmt.Errorf("copy Azure CLI auth: %w", err)
	}

	// Step 1: prerequisites (auth + azcmagent binary)
	if err := t.ensurePrerequisites(ctx); err != nil {
		return fmt.Errorf("arc prerequisites: %w", err)
	}

	// Step 2: SDK clients + registration + RBAC
	if err := t.execute(ctx); err != nil {
		return fmt.Errorf("arc installation: %w", err)
	}

	// Step 3: verify connectivity
	if !t.isCompleted(ctx) {
		return fmt.Errorf("arc installation completed but verification failed")
	}

	t.logger.Info("Arc installation completed successfully")
	return nil
}

// --- prerequisites ---

func (t *installArcTask) ensurePrerequisites(ctx context.Context) error {
	if err := t.ensureAuthentication(ctx); err != nil {
		return fmt.Errorf("authentication: %w", err)
	}

	if !isArcAgentInstalled() {
		if err := t.installArcAgentBinary(ctx); err != nil {
			return fmt.Errorf("install azcmagent binary: %w", err)
		}
	}
	return nil
}

func (t *installArcTask) ensureAuthentication(ctx context.Context) error {
	_, err := t.getCredential(ctx)
	return err
}

func (t *installArcTask) getCredential(ctx context.Context) (azcore.TokenCredential, error) {
	// Arc MSI → DefaultAzure → CLI
	if cred, err := t.authProvider.ArcCredential(); err == nil {
		if err := t.testCredential(ctx, cred); err == nil {
			return cred, nil
		}
	}
	if cred, err := azidentity.NewDefaultAzureCredential(nil); err == nil {
		if err := t.testCredential(ctx, cred); err == nil {
			return cred, nil
		}
	}
	if cred, err := azidentity.NewAzureCLICredential(nil); err == nil {
		if err := t.testCredential(ctx, cred); err == nil {
			return cred, nil
		}
	}
	return nil, fmt.Errorf("no valid Azure credential found")
}

func (t *installArcTask) testCredential(ctx context.Context, cred azcore.TokenCredential) error {
	_, err := t.authProvider.GetAccessToken(ctx, cred)
	return err
}

func (t *installArcTask) installArcAgentBinary(ctx context.Context) error {
	// Purge existing package state.
	_ = utilexec.RunCmdAt(ctx, t.logger, slog.LevelDebug, utilexec.Dpkg(), "--purge", "azcmagent") // best-effort

	tempDir, err := os.MkdirTemp("", "arc-install-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir) //nolint:errcheck // cleanup

	scriptPath := filepath.Join(tempDir, "install_linux_azcmagent.sh")
	if err := t.downloadArcInstallScript(ctx, scriptPath); err != nil {
		return err
	}
	if err := os.Chmod(scriptPath, 0o755); err != nil { //nolint:gosec // script needs to be executable
		return err
	}

	return utilexec.RunCmd(ctx, t.logger, utilexec.Bash(), scriptPath)
}

func (t *installArcTask) downloadArcInstallScript(ctx context.Context, dest string) error {
	if _, err := exec.LookPath("curl"); err == nil {
		return utilexec.RunCmd(ctx, t.logger, utilexec.Curl(), "-L", "-o", dest, arcInstallScriptURL)
	}
	if _, err := exec.LookPath("wget"); err == nil {
		return utilexec.RunCmd(ctx, t.logger, utilexec.Wget(), "-O", dest, arcInstallScriptURL)
	}
	return fmt.Errorf("neither curl nor wget available")
}

// --- execute ---

func (t *installArcTask) execute(ctx context.Context) error {
	if err := t.setUpClients(ctx); err != nil {
		return fmt.Errorf("setup clients: %w", err)
	}

	arcMachine, err := t.registerArcMachine(ctx)
	if err != nil {
		return fmt.Errorf("register arc machine: %w", err)
	}

	if err := t.validateManagedCluster(ctx); err != nil {
		return fmt.Errorf("validate managed cluster: %w", err)
	}

	// Brief pause for identity readiness before RBAC assignment.
	time.Sleep(10 * time.Second)

	if err := t.assignRBACRoles(ctx, arcMachine); err != nil {
		return fmt.Errorf("assign RBAC roles: %w", err)
	}

	return nil
}

func (t *installArcTask) setUpClients(ctx context.Context) error {
	cred, err := t.getCredential(ctx)
	if err != nil {
		return fmt.Errorf("obtain credential: %w", err)
	}

	subID := t.cfg.Azure.SubscriptionID

	t.hybridComputeClient, err = armhybridcompute.NewMachinesClient(subID, cred, nil)
	if err != nil {
		return fmt.Errorf("create hybrid compute client: %w", err)
	}
	t.mcClient, err = armcontainerservice.NewManagedClustersClient(subID, cred, nil)
	if err != nil {
		return fmt.Errorf("create managed clusters client: %w", err)
	}
	t.roleAssignmentsClient, err = armauthorization.NewRoleAssignmentsClient(subID, cred, nil)
	if err != nil {
		return fmt.Errorf("create role assignments client: %w", err)
	}
	return nil
}

// --- registration ---

func (t *installArcTask) registerArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	machine, err := t.getArcMachine(ctx)
	if err == nil && machine != nil {
		t.logger.Info("machine already registered with Arc", "name", ptrDeref(machine.Name))
		return machine, nil
	}

	if err := t.runArcAgentConnect(ctx); err != nil {
		return nil, fmt.Errorf("azcmagent connect: %w", err)
	}

	return t.waitForArcRegistration(ctx)
}

func (t *installArcTask) getArcMachine(ctx context.Context) (*armhybridcompute.Machine, error) {
	result, err := t.hybridComputeClient.Get(ctx, t.cfg.GetArcResourceGroup(), t.cfg.GetArcMachineName(), nil)
	if err != nil {
		return nil, err
	}
	return &result.Machine, nil
}

func (t *installArcTask) waitForArcRegistration(ctx context.Context) (*armhybridcompute.Machine, error) {
	const (
		maxRetries   = 10
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	for attempt := range maxRetries {
		machine, err := t.getArcMachine(ctx)
		if err == nil && machine != nil && machine.Identity != nil && machine.Identity.PrincipalID != nil {
			return machine, nil
		}
		t.logger.Info("arc registration not ready, retrying", "attempt", attempt+1, "maxRetries", maxRetries)

		delay := min(initialDelay*time.Duration(1<<attempt), maxDelay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("arc registration timed out after %d attempts", maxRetries)
}

func (t *installArcTask) runArcAgentConnect(ctx context.Context) error {
	cfg := t.cfg
	args := []string{
		"connect",
		"--resource-group", cfg.GetArcResourceGroup(),
		"--tenant-id", cfg.Azure.TenantID,
		"--location", cfg.GetArcLocation(),
		"--subscription-id", cfg.Azure.SubscriptionID,
		"--resource-name", cfg.GetArcMachineName(),
	}

	for key, value := range cfg.GetArcTags() {
		args = append(args, "--tags", fmt.Sprintf("%s=%s", key, value))
	}

	// Add access token for authentication.
	cred, err := t.getCredential(ctx)
	if err != nil {
		return fmt.Errorf("obtain credential for azcmagent: %w", err)
	}
	accessToken, err := t.authProvider.GetAccessToken(ctx, cred)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	args = append(args, "--access-token", accessToken)

	// Log masked command.
	masked := make([]string, len(args))
	copy(masked, args)
	for i := 0; i < len(masked)-1; i++ {
		if masked[i] == "--access-token" {
			masked[i+1] = "***REDACTED***"
			break
		}
	}
	t.logger.Info("running azcmagent connect", "args", masked)

	_, err = utilexec.OutputCmd(ctx, t.logger, "azcmagent", args...)
	return err
}

// --- cluster validation ---

func (t *installArcTask) validateManagedCluster(ctx context.Context) error {
	cfg := t.cfg
	clusterRG := cfg.GetArcResourceGroup()
	clusterName := cfg.GetTargetClusterName()

	result, err := t.mcClient.Get(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return fmt.Errorf("get AKS cluster: %w", err)
	}

	cluster := result.ManagedCluster
	if cluster.Properties == nil ||
		cluster.Properties.AADProfile == nil ||
		cluster.Properties.AADProfile.EnableAzureRBAC == nil ||
		!*cluster.Properties.AADProfile.EnableAzureRBAC {
		return fmt.Errorf("target AKS cluster %q must have Azure RBAC enabled", clusterName)
	}

	return nil
}

// --- RBAC ---

func (t *installArcTask) assignRBACRoles(ctx context.Context, arcMachine *armhybridcompute.Machine) error {
	principalID := getArcMachineIdentityID(arcMachine)
	if principalID == "" {
		return fmt.Errorf("managed identity ID not found on Arc machine")
	}

	roles := t.getRoleAssignments()
	var assignmentErrors []error
	for i, role := range roles {
		t.logger.Info("assigning RBAC role", "index", i+1, "total", len(roles), "role", role.roleName, "scope", role.scope)
		if err := t.assignRole(ctx, principalID, role.roleID, role.scope, role.roleName); err != nil {
			t.logger.Error("failed to assign role", "role", role.roleName, "error", err)
			assignmentErrors = append(assignmentErrors, fmt.Errorf("role %q: %w", role.roleName, err))
		}
	}

	if len(assignmentErrors) > 0 {
		return fmt.Errorf("failed to assign %d/%d RBAC roles", len(assignmentErrors), len(roles))
	}

	// Wait for permissions to propagate.
	t.logger.Info("waiting for RBAC permissions to propagate", "principalID", principalID)
	return t.waitForPermissions(ctx, principalID)
}

func (t *installArcTask) getRoleAssignments() []roleAssignment {
	cfg := t.cfg
	subID := cfg.Azure.SubscriptionID
	rg := cfg.GetArcResourceGroup()
	clusterName := cfg.GetTargetClusterName()

	subScope := fmt.Sprintf("/subscriptions/%s", subID)
	rgScope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subID, rg)
	clusterScope := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s", subID, rg, clusterName)

	return []roleAssignment{
		{"Reader", subScope, roleDefinitionIDs["Reader"]},
		{"Contributor", rgScope, roleDefinitionIDs["Contributor"]},
		{"Azure Kubernetes Service RBAC Cluster Admin", clusterScope, roleDefinitionIDs["Azure Kubernetes Service RBAC Cluster Admin"]},
		{"Azure Kubernetes Service Cluster Admin Role", clusterScope, roleDefinitionIDs["Azure Kubernetes Service Cluster Admin Role"]},
	}
}

func (t *installArcTask) assignRole(ctx context.Context, principalID, roleDefID, scope, roleName string) error {
	fullRoleDefID := fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Authorization/roleDefinitions/%s",
		t.cfg.Azure.SubscriptionID, roleDefID)

	const (
		maxRetries   = 5
		initialDelay = 5 * time.Second
		maxDelay     = 30 * time.Second
	)

	var lastErr error
	for attempt := range maxRetries {
		if attempt > 0 {
			delay := min(initialDelay*time.Duration(1<<(attempt-1)), maxDelay)
			t.logger.Info("retrying role assignment", "delay", delay, "attempt", attempt+1)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		principalType := armauthorization.PrincipalTypeServicePrincipal
		assignment := armauthorization.RoleAssignmentCreateParameters{
			Properties: &armauthorization.RoleAssignmentProperties{
				PrincipalID:      &principalID,
				RoleDefinitionID: &fullRoleDefID,
				PrincipalType:    &principalType,
			},
		}

		name := uuid.New().String()
		if _, err := t.roleAssignmentsClient.Create(ctx, scope, name, assignment, nil); err != nil {
			lastErr = err
			errStr := err.Error()

			if strings.Contains(errStr, "403") || strings.Contains(errStr, "Forbidden") {
				return fmt.Errorf("insufficient permissions to assign role %q: %w", roleName, err)
			}
			if strings.Contains(errStr, "RoleAssignmentExists") {
				return nil
			}
			if strings.Contains(errStr, "PrincipalNotFound") {
				continue // retriable
			}
			return fmt.Errorf("create role assignment for %q: %w", roleName, err)
		}
		return nil
	}

	return fmt.Errorf("role assignment for %q failed after %d attempts: %w", roleName, maxRetries, lastErr)
}

func (t *installArcTask) waitForPermissions(ctx context.Context, _ string) error {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	timeout := time.After(10 * time.Minute)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("context cancelled waiting for permissions: %w", ctx.Err())
		case <-timeout:
			return fmt.Errorf("timeout waiting for RBAC permissions")
		case <-ticker.C:
			if _, err := t.getArcMachine(ctx); err == nil {
				t.logger.Info("RBAC permissions propagated")
				return nil
			}
			t.logger.Info("permissions not yet propagated, retrying")
		}
	}
}

// --- isCompleted ---

func (t *installArcTask) isCompleted(ctx context.Context) bool {
	if !isArcServicesRunning(ctx, t.logger) {
		return false
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	output, err := utilexec.OutputCmdAt(timeoutCtx, t.logger, slog.LevelDebug, "azcmagent", "show")
	if err != nil {
		return false
	}

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Agent Status") && strings.Contains(line, ":") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.ToLower(strings.TrimSpace(parts[1])) == "connected"
			}
		}
	}
	return false
}
