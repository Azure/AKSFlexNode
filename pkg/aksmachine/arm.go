package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

type armMachineClient struct {
	machineID *arm.ResourceID
	client    *armcontainerservice.MachinesClient
	logger    *slog.Logger
}

// NewARMClient returns a MachineClient backed by the AKS ARM Machine API.
func NewARMClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	machineID, err := machineResourceIDFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	cred, err := getCredential(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("resolve ARM credential: %w", err)
	}
	var armOpts *arm.ClientOptions // nil = default public ARM endpoint
	client, err := armcontainerservice.NewMachinesClient(machineID.SubscriptionID, cred, armOpts)
	if err != nil {
		return nil, fmt.Errorf("create machines client: %w", err)
	}
	return &armMachineClient{
		machineID: machineID,
		client:    client,
		logger:    logger,
	}, nil
}

func (c *armMachineClient) Create(ctx context.Context, desired GoalState) (*Machine, error) {
	if err := desired.validate(); err != nil {
		return nil, fmt.Errorf("validate goal state: %w", err)
	}
	params := armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Tags: map[string]*string{
				flexNodeTagKey: new("true"),
			},
			Kubernetes: buildK8sProfile(desired),
		},
	}
	agentPoolID := c.machineID.Parent
	clusterID := agentPoolID.Parent
	c.logger.Info("creating or updating AKS machine", "machine", c.machineID.Name, "pool", agentPoolID.Name)
	poller, err := c.client.BeginCreateOrUpdate(
		ctx,
		c.machineID.ResourceGroupName,
		clusterID.Name,
		agentPoolID.Name,
		c.machineID.Name,
		params,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("begin create machine %q: %w", c.machineID.Name, err)
	}
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("wait for machine %q: %w", c.machineID.Name, err)
	}
	if err := c.validateMachineIdentity(resp.Machine); err != nil {
		return nil, err
	}
	result := machineFromARM(resp.Machine, desired)
	result.ID = c.machineID.String()
	result.Name = c.machineID.Name
	return result, nil
}

func (c *armMachineClient) Get(ctx context.Context) (*Machine, error) {
	agentPoolID := c.machineID.Parent
	clusterID := agentPoolID.Parent
	resp, err := c.client.Get(
		ctx,
		c.machineID.ResourceGroupName,
		clusterID.Name,
		agentPoolID.Name,
		c.machineID.Name,
		nil,
	)
	if isARMNotFound(err) {
		return nil, &NotFoundError{Resource: c.machineID.String()}
	}
	if err != nil {
		return nil, fmt.Errorf("get machine %q: %w", c.machineID.Name, err)
	}
	if err := c.validateMachineIdentity(resp.Machine); err != nil {
		return nil, err
	}
	result := machineFromARM(resp.Machine, GoalState{})
	result.ID = c.machineID.String()
	result.Name = c.machineID.Name
	return result, nil
}

func (c *armMachineClient) PatchStatus(context.Context, Status) error {
	// TODO: impement this
	c.logger.Warn("skipping AKS machine status update; ARM Machine status is read-only")
	return nil
}

func machineResourceIDFromConfig(cfg *config.Config) (*arm.ResourceID, error) {
	if cfg.Azure.TargetCluster.ResourceID == "" || cfg.Agent.NodeName == "" || cfg.Kubernetes.Version == "" {
		return nil, fmt.Errorf("incomplete AKS machine config: clusterResourceId=%q machineName=%q kubernetesVersion=%q",
			cfg.Azure.TargetCluster.ResourceID, cfg.Agent.NodeName, cfg.Kubernetes.Version)
	}
	machineResourceID := strings.TrimRight(cfg.Azure.TargetCluster.ResourceID, "/") + "/agentPools/" + aksFlexNodePoolName + "/machines/" + cfg.Agent.NodeName
	machineID, err := arm.ParseResourceID(machineResourceID)
	if err != nil {
		return nil, fmt.Errorf("parse AKS machine resource ID %q: %w", machineResourceID, err)
	}
	return machineID, nil
}

func getCredential(cfg *config.Config, logger *slog.Logger) (azcore.TokenCredential, error) {
	switch {
	case cfg.IsSPConfigured():
		logger.Debug(
			"using service principal credential for ARM",
			"tenantID", cfg.Azure.ServicePrincipal.TenantID,
			"clientID", cfg.Azure.ServicePrincipal.ClientID,
		)
		return azidentity.NewClientSecretCredential(
			cfg.Azure.ServicePrincipal.TenantID,
			cfg.Azure.ServicePrincipal.ClientID,
			cfg.Azure.ServicePrincipal.ClientSecret,
			nil,
		)
	case cfg.IsMIConfigured():
		opts := &azidentity.ManagedIdentityCredentialOptions{}
		if cfg.Azure.ManagedIdentity != nil && cfg.Azure.ManagedIdentity.ClientID != "" {
			opts.ID = azidentity.ClientID(cfg.Azure.ManagedIdentity.ClientID)
			logger.Debug(
				"using user-assigned managed identity credential for ARM",
				"clientID", cfg.Azure.ManagedIdentity.ClientID,
			)
		} else {
			logger.Debug("using system-assigned managed identity credential for ARM")
		}
		return azidentity.NewManagedIdentityCredential(opts)
	default:
		logger.Debug("falling back to default credential for ARM")
		return azidentity.NewDefaultAzureCredential(nil)
	}
}

func buildK8sProfile(goal GoalState) *armcontainerservice.MachineKubernetesProfile {
	p := &armcontainerservice.MachineKubernetesProfile{
		OrchestratorVersion: &goal.KubernetesVersion,
		MaxPods:             new(int32(goal.MaxPods)), //nolint:gosec // validated non-negative and small
		NodeLabels:          stringPointerMap(maps.Clone(goal.NodeLabels)),
		NodeTaints:          stringPointerSlice(slices.Clone(goal.NodeTaints)),
		KubeletConfig: &armcontainerservice.KubeletConfig{
			ImageGcHighThreshold: new(int32(goal.KubeletConfig.ImageGCHighThreshold)), //nolint:gosec // validated non-negative and small
			ImageGcLowThreshold:  new(int32(goal.KubeletConfig.ImageGCLowThreshold)),  //nolint:gosec // validated non-negative and small
		},
	}
	return p
}

func stringPointerMap(values map[string]string) map[string]*string {
	if values == nil {
		return nil
	}
	result := make(map[string]*string, len(values))
	for k, v := range values {
		value := v
		result[k] = &value
	}
	return result
}

func stringPointerSlice(values []string) []*string {
	if values == nil {
		return nil
	}
	result := make([]*string, len(values))
	for i, v := range values {
		value := v
		result[i] = &value
	}
	return result
}

func (c *armMachineClient) validateMachineIdentity(machine armcontainerservice.Machine) error {
	if machine.ID != nil && !strings.EqualFold(*machine.ID, c.machineID.String()) {
		return fmt.Errorf("AKS machine ID mismatch: got %q, want %q", *machine.ID, c.machineID.String())
	}
	if machine.Name != nil && *machine.Name != c.machineID.Name {
		return fmt.Errorf("AKS machine name mismatch: got %q, want %q", *machine.Name, c.machineID.Name)
	}
	return nil
}

func machineFromARM(machine armcontainerservice.Machine, fallback GoalState) *Machine {
	result := &Machine{Goal: fallback}
	if machine.Properties != nil {
		if machine.Properties.Kubernetes != nil {
			if machine.Properties.Kubernetes.OrchestratorVersion != nil {
				result.Goal.KubernetesVersion = *machine.Properties.Kubernetes.OrchestratorVersion
			}
			if result.Goal.KubernetesVersion == "" && machine.Properties.Kubernetes.CurrentOrchestratorVersion != nil {
				result.Goal.KubernetesVersion = *machine.Properties.Kubernetes.CurrentOrchestratorVersion
			}
		}
		if result.Goal.SettingsVersion == "" {
			result.Goal.SettingsVersion = result.Goal.KubernetesVersion
		}
		if machine.Properties.ProvisioningState != nil {
			result.Status.ProvisioningState = ProvisioningState(*machine.Properties.ProvisioningState)
		}
	}
	return result
}

func isARMNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

var _ MachineClient = (*armMachineClient)(nil)
