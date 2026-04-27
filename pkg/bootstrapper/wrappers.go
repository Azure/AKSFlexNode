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
	"path/filepath"
	"strings"
	"text/template"

	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
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
