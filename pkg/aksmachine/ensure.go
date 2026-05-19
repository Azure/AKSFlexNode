package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	aksFlexNodePoolName = "aksflexnodes"
	flexNodeTagKey      = "aks-flex-node"
)

type ensureMachineTask struct {
	cfg    *config.Config
	logger *slog.Logger
}

// EnsureMachine returns a task that ensures the AKS "aksflexnodes" agent pool
// (mode=Machines) exists and this machine is registered in it.
func EnsureMachine(cfg *config.Config, logger *slog.Logger) phases.Task {
	return &ensureMachineTask{cfg: cfg, logger: logger}
}

func (t *ensureMachineTask) Name() string { return "ensure-machine" }

func (t *ensureMachineTask) Do(ctx context.Context) error {
	subID := t.cfg.Azure.TargetCluster.SubscriptionID
	rg := t.cfg.Azure.TargetCluster.ResourceGroup
	clusterName := t.cfg.Azure.TargetCluster.Name
	machineName := t.cfg.Agent.NodeName // TODO: add support for overriding machine name in config
	k8sVersion := t.cfg.Kubernetes.Version

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
