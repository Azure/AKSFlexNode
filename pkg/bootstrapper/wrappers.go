package bootstrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

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
