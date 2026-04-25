package bootstrapper

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/authorization/armauthorization/v3"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"github.com/google/uuid"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/pkg/auth"
	pkgarc "github.com/Azure/AKSFlexNode/pkg/components/arc"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilhost"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---------------------------------------------------------------------------
// NPD: Download
// ---------------------------------------------------------------------------

const (
	defaultNPDURLTemplate = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"

	npdBinaryPath = "/usr/bin/node-problem-detector"
	npdConfigPath = "/etc/node-problem-detector/kernel-monitor.json"
)

type downloadNPDTask struct {
	version string
}

// DownloadNPD returns a task that downloads the node-problem-detector binary
// and config from the upstream GitHub release tarball.
func DownloadNPD(cfg *config.Config) phases.Task {
	version := cfg.Npd.Version
	if version == "" {
		version = config.DefaultNPDVersion
	}
	return &downloadNPDTask{version: version}
}

func (t *downloadNPDTask) Name() string { return "download-npd" }

func (t *downloadNPDTask) Do(ctx context.Context) error {
	if npdVersionMatch(t.version) {
		return nil // already installed at correct version
	}

	downloadURL := constructNPDDownloadURL(t.version)
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress npd tar: %w", err)
		}

		switch tarFile.Name {
		case "bin/node-problem-detector":
			if err := utilio.InstallFile(npdBinaryPath, tarFile.Body, 0o755); err != nil { //nolint:gosec // binary must be executable
				return fmt.Errorf("install npd binary: %w", err)
			}
		case "config/kernel-monitor.json":
			if err := utilio.InstallFile(npdConfigPath, tarFile.Body, 0o644); err != nil { //nolint:gosec // config must be readable
				return fmt.Errorf("install npd config: %w", err)
			}
		default:
			continue
		}
	}

	return nil
}

func constructNPDDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultNPDURLTemplate, version, version, arch)
}

func npdVersionMatch(expectedVersion string) bool {
	if !utilio.IsExecutable(npdBinaryPath) {
		return false
	}
	output, err := utilexec.New().Command(npdBinaryPath, "--version").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), expectedVersion)
}

// ---------------------------------------------------------------------------
// NPD: Start (systemd service)
// ---------------------------------------------------------------------------

//go:embed assets/node-problem-detector.service
var npdServiceTemplate string

const (
	systemdUnitNPD = "node-problem-detector.service"
	npdServicePath = "/etc/systemd/system/node-problem-detector.service"
)

var npdTmpl = template.Must(template.New("npd-service").Parse(npdServiceTemplate))

type startNPDTask struct {
	apiServer      string
	kubeconfigPath string
	systemd        systemd.Manager
}

// StartNPD returns a task that renders the NPD systemd unit file and ensures
// the service is running.
func StartNPD(cfg *config.Config) phases.Task {
	return &startNPDTask{
		apiServer:      cfg.Node.Kubelet.ServerURL,
		kubeconfigPath: config.KubeletKubeconfigPath,
		systemd:        systemd.New(),
	}
}

func (t *startNPDTask) Name() string { return "start-npd" }

func (t *startNPDTask) Do(ctx context.Context) error {
	serviceUpdated, err := t.ensureServiceFile()
	if err != nil {
		return fmt.Errorf("ensure npd service file: %w", err)
	}

	return t.ensureSystemdUnit(ctx, serviceUpdated)
}

func (t *startNPDTask) ensureServiceFile() (updated bool, err error) {
	var buf bytes.Buffer
	if err := npdTmpl.Execute(&buf, map[string]any{
		"NPDBinaryPath":  npdBinaryPath,
		"APIServerURL":   t.apiServer,
		"KubeconfigPath": t.kubeconfigPath,
		"NPDConfigPath":  npdConfigPath,
	}); err != nil {
		return false, fmt.Errorf("render npd service template: %w", err)
	}

	current, err := os.ReadFile(npdServicePath) //nolint:gosec // path is constructed, not user input
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fall through to create
	case err != nil:
		return false, err
	default:
		if bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(buf.Bytes())) {
			return false, nil
		}
	}

	if err := utilio.InstallFile(npdServicePath, &buf, 0o644); err != nil { //nolint:gosec // service files must be world-readable
		return false, err
	}
	return true, nil
}

func (t *startNPDTask) ensureSystemdUnit(ctx context.Context, restart bool) error {
	_, err := t.systemd.GetUnitStatus(ctx, systemdUnitNPD)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		if err := t.systemd.DaemonReload(ctx); err != nil {
			return err
		}
		return t.systemd.StartUnit(ctx, systemdUnitNPD)
	case err != nil:
		return err
	default:
		if restart {
			return t.systemd.ReloadOrRestartUnit(ctx, systemdUnitNPD)
		}
		return nil
	}
}

// ---------------------------------------------------------------------------
// Arc: Install
// ---------------------------------------------------------------------------

const arcInstallScriptURL = "https://gbl.his.arc.azure.com/azcmagent-linux"

// Arc services that may be present.
var arcServices = []string{"himdsd", "gcarcservice", "extd"}

// Map role names to role definition IDs.
var roleDefinitionIDs = map[string]string{
	"Reader":      "acdd72a7-3385-48ef-bd42-f606fba81ae7",
	"Contributor": "b24988ac-6180-42a0-ab88-20f7382dd24c",
	"Azure Kubernetes Service RBAC Cluster Admin": "b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b",
	"Azure Kubernetes Service Cluster Admin Role": "0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8",
}

type roleAssignment struct {
	roleName string
	scope    string
	roleID   string
}

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
	cmd := exec.CommandContext(ctx, "dpkg", "--purge", "azcmagent") //nolint:gosec // constant args
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run() // best-effort

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

	installCmd := exec.CommandContext(ctx, "bash", scriptPath) //nolint:gosec // trusted script
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	return installCmd.Run()
}

func (t *installArcTask) downloadArcInstallScript(ctx context.Context, dest string) error {
	if _, err := exec.LookPath("curl"); err == nil {
		return exec.CommandContext(ctx, "curl", "-L", "-o", dest, arcInstallScriptURL).Run() //nolint:gosec // constant URL
	}
	if _, err := exec.LookPath("wget"); err == nil {
		return exec.CommandContext(ctx, "wget", "-O", dest, arcInstallScriptURL).Run() //nolint:gosec // constant URL
	}
	return fmt.Errorf("neither curl nor wget available")
}

// --- execute ---

func (t *installArcTask) execute(ctx context.Context) error {
	cfg := t.cfg

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

	_ = cfg // suppress unused warning if needed
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

	_, err = utils.RunCommandWithOutput("azcmagent", args...)
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
	if !isArcServicesRunning(ctx) {
		return false
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, "azcmagent", "show")
	output, err := cmd.Output()
	if err != nil {
		return false
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
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

// --- Arc helpers ---

func isArcAgentInstalled() bool {
	_, err := exec.LookPath("azcmagent")
	return err == nil
}

func isArcServicesRunning(ctx context.Context) bool {
	if !isArcAgentInstalled() {
		return false
	}
	for _, service := range arcServices {
		if !utils.IsServiceActive(service) {
			return false
		}
	}
	cmd := exec.CommandContext(ctx, "pgrep", "-f", "azcmagent")
	return cmd.Run() == nil
}

func getArcMachineIdentityID(m *armhybridcompute.Machine) string {
	if m != nil && m.Identity != nil && m.Identity.PrincipalID != nil {
		return *m.Identity.PrincipalID
	}
	return ""
}

func ptrDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// ---------------------------------------------------------------------------
// Arc: Uninstall (delegates to existing pkg/components/arc uninstaller)
// ---------------------------------------------------------------------------

type uninstallArcTask struct {
	logger *slog.Logger
}

// UninstallArc returns a task that unregisters the machine from Azure Arc and
// cleans up all Arc-related resources.
func UninstallArc(logger *slog.Logger) phases.Task {
	return &uninstallArcTask{logger: logger}
}

func (t *uninstallArcTask) Name() string { return "uninstall-arc" }

func (t *uninstallArcTask) Do(ctx context.Context) error {
	u := pkgarc.NewUnInstaller(toLogrus(t.logger))
	return u.Execute(ctx)
}

// ---------------------------------------------------------------------------
// Enrich Cluster Config
// ---------------------------------------------------------------------------

type enrichClusterConfigTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

// EnrichClusterConfig returns a task that fetches cluster metadata (server URL,
// CA cert) from AKS for non-bootstrap-token auth modes.
func EnrichClusterConfig(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &enrichClusterConfigTask{cfg: cfg, logger: logger}
}

func (t *enrichClusterConfigTask) Name() string { return "enrich-cluster-config" }

func (t *enrichClusterConfigTask) Do(ctx context.Context) error {
	enricher := newClusterConfigEnricher(t.cfg, toLogrus(t.logger))
	if enricher.IsCompleted(ctx) {
		return nil
	}
	return enricher.Execute(ctx)
}

// ---------------------------------------------------------------------------
// CNI Config
// ---------------------------------------------------------------------------

//go:embed assets/99-bridge.conf
var defaultBridgeCNIConfig []byte

type writeCNIConfigTask struct {
	machineDir string
}

// WriteCNIConfig returns a task that writes the default bridge CNI config
// into the nspawn rootfs at /etc/cni/net.d/99-bridge.conf.
func WriteCNIConfig(machineDir string) phases.Task {
	return &writeCNIConfigTask{machineDir: machineDir}
}

func (t *writeCNIConfigTask) Name() string { return "write-cni-config" }

func (t *writeCNIConfigTask) Do(_ context.Context) error {
	confDir := filepath.Join(t.machineDir, "etc", "cni", "net.d")
	if err := os.MkdirAll(confDir, 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create CNI config directory: %w", err)
	}

	confPath := filepath.Join(confDir, "99-bridge.conf")

	current, err := os.ReadFile(confPath) //nolint:gosec // path is constructed, not user input
	if err == nil && string(current) == string(defaultBridgeCNIConfig) {
		return nil
	}

	if err := os.WriteFile(confPath, defaultBridgeCNIConfig, 0o644); err != nil { //nolint:gosec // CNI config must be world-readable
		return fmt.Errorf("write CNI bridge config: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Install Binary (copy aks-flex-node into nspawn rootfs)
// ---------------------------------------------------------------------------

type installBinaryTask struct {
	machineDir string
}

// InstallBinary returns a task that copies the current process binary into
// the nspawn rootfs at /usr/local/bin/aks-flex-node.
func InstallBinary(machineDir string) phases.Task {
	return &installBinaryTask{machineDir: machineDir}
}

func (t *installBinaryTask) Name() string { return "install-binary-in-rootfs" }

func (t *installBinaryTask) Do(_ context.Context) error {
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self executable: %w", err)
	}

	destPath := filepath.Join(t.machineDir, "usr", "local", "bin", "aks-flex-node")
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil { //nolint:gosec // directory needs to be traversable
		return fmt.Errorf("create destination directory: %w", err)
	}

	src, err := os.Open(selfPath) //nolint:gosec // path is from os.Executable(), not user input
	if err != nil {
		return fmt.Errorf("open self binary: %w", err)
	}
	defer func() { _ = src.Close() }()

	dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o750) //nolint:gosec // binary must be executable
	if err != nil {
		return fmt.Errorf("create destination binary: %w", err)
	}
	defer func() { _ = dst.Close() }()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy binary: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// AKS Machine: Ensure machine resource is registered in the AKS Machines API
// ---------------------------------------------------------------------------

const (
	aksFlexNodePoolName = "aksflexnodes"
	flexNodeTagKey      = "aks-flex-node"
)

type ensureMachineTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

// EnsureMachine returns a task that ensures the AKS "aksflexnodes" agent pool
// (mode=Machines) exists and this machine is registered in it. It is a no-op
// when drift detection/remediation is disabled in the config.
func EnsureMachine(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{cfg: cfg, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	if !t.cfg.IsDriftDetectionAndRemediationEnabled() {
		t.logger.Info("drift detection/remediation disabled, skipping ensure-machine")
		return nil
	}

	cfg := t.cfg
	subID := cfg.GetTargetClusterSubscriptionID()
	rg := cfg.GetTargetClusterResourceGroup()
	clusterName := cfg.GetTargetClusterName()
	machineName := cfg.GetArcMachineName() // hostname-based
	k8sVersion := cfg.GetKubernetesVersion()

	if subID == "" || rg == "" || clusterName == "" || machineName == "" || k8sVersion == "" {
		return fmt.Errorf("ensure-machine: incomplete config: subscriptionId=%q resourceGroup=%q clusterName=%q machineName=%q kubernetesVersion=%q",
			subID, rg, clusterName, machineName, k8sVersion)
	}

	cred, err := t.getCredential()
	if err != nil {
		return fmt.Errorf("ensure-machine: resolve credential: %w", err)
	}

	var armOpts *arm.ClientOptions // nil = default public ARM endpoint

	// Step 1: ensure agent pool exists.
	if err := t.ensureAgentPool(ctx, cred, armOpts, subID, rg, clusterName); err != nil {
		return fmt.Errorf("ensure-machine: ensure agent pool: %w", err)
	}

	// Step 2: ensure this machine is registered.
	if err := t.ensureMachineResource(ctx, cred, armOpts, subID, rg, clusterName, machineName, k8sVersion); err != nil {
		return fmt.Errorf("ensure-machine: ensure machine: %w", err)
	}

	return nil
}

func (t *ensureMachineTask) getCredential() (azcore.TokenCredential, error) {
	cfg := t.cfg
	if cfg.IsSPConfigured() {
		return azidentity.NewClientSecretCredential(
			cfg.Azure.ServicePrincipal.TenantID,
			cfg.Azure.ServicePrincipal.ClientID,
			cfg.Azure.ServicePrincipal.ClientSecret,
			nil,
		)
	}
	if cfg.IsMIConfigured() {
		opts := &azidentity.ManagedIdentityCredentialOptions{}
		if cfg.Azure.ManagedIdentity != nil && cfg.Azure.ManagedIdentity.ClientID != "" {
			opts.ID = azidentity.ClientID(cfg.Azure.ManagedIdentity.ClientID)
		}
		return azidentity.NewManagedIdentityCredential(opts)
	}
	return azidentity.NewAzureCLICredential(nil)
}

func (t *ensureMachineTask) ensureAgentPool(
	ctx context.Context,
	cred azcore.TokenCredential,
	armOpts *arm.ClientOptions,
	subID, rg, clusterName string,
) error {
	client, err := armcontainerservice.NewAgentPoolsClient(subID, cred, armOpts)
	if err != nil {
		return fmt.Errorf("create agent pools client: %w", err)
	}

	// Skip if already exists.
	if _, err := client.Get(ctx, rg, clusterName, aksFlexNodePoolName, nil); err == nil {
		t.logger.Info("agent pool already exists, skipping", "pool", aksFlexNodePoolName)
		return nil
	} else if !isARMNotFound(err) {
		return fmt.Errorf("get agent pool %q: %w", aksFlexNodePoolName, err)
	}

	mode := armcontainerservice.AgentPoolMode("Machines")
	params := armcontainerservice.AgentPool{
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			Mode: &mode,
		},
	}

	t.logger.Info("creating agent pool", "pool", aksFlexNodePoolName, "cluster", clusterName)
	poller, err := client.BeginCreateOrUpdate(ctx, rg, clusterName, aksFlexNodePoolName, params, nil)
	if err != nil {
		return fmt.Errorf("begin create agent pool %q: %w", aksFlexNodePoolName, err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for agent pool %q: %w", aksFlexNodePoolName, err)
	}

	t.logger.Info("agent pool created", "pool", aksFlexNodePoolName)
	return nil
}

func (t *ensureMachineTask) ensureMachineResource(
	ctx context.Context,
	cred azcore.TokenCredential,
	armOpts *arm.ClientOptions,
	subID, rg, clusterName, machineName, k8sVersion string,
) error {
	client, err := armcontainerservice.NewMachinesClient(subID, cred, armOpts)
	if err != nil {
		return fmt.Errorf("create machines client: %w", err)
	}

	// Skip if already exists.
	if _, err := client.Get(ctx, rg, clusterName, aksFlexNodePoolName, machineName, nil); err == nil {
		t.logger.Info("machine already registered, skipping", "machine", machineName)
		return nil
	} else if !isARMNotFound(err) {
		return fmt.Errorf("get machine %q: %w", machineName, err)
	}

	params := armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Tags: map[string]*string{
				flexNodeTagKey: to.Ptr("true"),
			},
			Kubernetes: t.buildK8sProfile(k8sVersion),
		},
	}

	t.logger.Info("registering machine", "machine", machineName, "pool", aksFlexNodePoolName)
	poller, err := client.BeginCreateOrUpdate(ctx, rg, clusterName, aksFlexNodePoolName, machineName, params, nil)
	if err != nil {
		return fmt.Errorf("begin create machine %q: %w", machineName, err)
	}
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for machine %q: %w", machineName, err)
	}

	t.logger.Info("machine registered", "machine", machineName)
	return nil
}

func (t *ensureMachineTask) buildK8sProfile(k8sVersion string) *armcontainerservice.MachineKubernetesProfile {
	cfg := t.cfg
	p := &armcontainerservice.MachineKubernetesProfile{}

	if k8sVersion != "" {
		p.OrchestratorVersion = to.Ptr(k8sVersion)
	}
	if cfg.Node.MaxPods > 0 {
		p.MaxPods = to.Ptr(int32(cfg.Node.MaxPods)) //nolint:gosec // max pods is always a small positive int
	}
	if len(cfg.Node.Labels) > 0 {
		p.NodeLabels = make(map[string]*string, len(cfg.Node.Labels))
		for k, v := range cfg.Node.Labels {
			p.NodeLabels[k] = to.Ptr(v)
		}
	}
	if len(cfg.Node.Taints) > 0 {
		p.NodeTaints = make([]*string, len(cfg.Node.Taints))
		for i, taint := range cfg.Node.Taints {
			p.NodeTaints[i] = to.Ptr(taint)
		}
	}

	// Image GC thresholds.
	if h := cfg.Node.Kubelet.ImageGCHighThreshold; h > 0 {
		if p.KubeletConfig == nil {
			p.KubeletConfig = &armcontainerservice.KubeletConfig{}
		}
		p.KubeletConfig.ImageGcHighThreshold = to.Ptr(int32(h)) //nolint:gosec // threshold is always small
	}
	if l := cfg.Node.Kubelet.ImageGCLowThreshold; l > 0 {
		if p.KubeletConfig == nil {
			p.KubeletConfig = &armcontainerservice.KubeletConfig{}
		}
		p.KubeletConfig.ImageGcLowThreshold = to.Ptr(int32(l)) //nolint:gosec // threshold is always small
	}

	return p
}

// isARMNotFound reports whether the Azure SDK error is an HTTP 404.
func isARMNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}
