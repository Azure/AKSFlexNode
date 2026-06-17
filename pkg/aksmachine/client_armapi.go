package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/azclient"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

type armMachineClient struct {
	machineID *arm.ResourceID
	client    *armcontainerservice.MachinesClient
	logger    *slog.Logger
}

// newARMClient returns a MachineClient backed by the AKS ARM Machine API.
func newARMClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	machineID, err := machineResourceIDFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	clientOpts := azureClientOptionsFromConfig(cfg)
	cred, err := getCredential(cfg, logger, clientOpts)
	if err != nil {
		return nil, fmt.Errorf("resolve ARM credential: %w", err)
	}
	armOpts := &arm.ClientOptions{ClientOptions: clientOpts}
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
	// TODO: implement this.
	c.logger.Warn("skipping AKS machine status update; ARM Machine status is read-only")
	return nil
}

func machineResourceIDFromConfig(cfg *config.Config) (*arm.ResourceID, error) {
	var clusterResourceID, agentPoolName, machineName, k8sVersion string
	if cfg != nil {
		if cfg.Azure.TargetCluster != nil {
			clusterResourceID = cfg.Azure.TargetCluster.ResourceID
		}
		agentPoolName = strings.TrimSpace(cfg.Azure.TargetAgentPoolName)
		machineName = cfg.Agent.NodeName
		k8sVersion = cfg.Kubernetes.Version
	}
	if clusterResourceID == "" || agentPoolName == "" || machineName == "" || k8sVersion == "" {
		return nil, fmt.Errorf("incomplete AKS machine config: clusterResourceId=%q targetAgentPoolName=%q machineName=%q kubernetesVersion=%q",
			clusterResourceID, agentPoolName, machineName, k8sVersion)
	}
	clusterID, err := arm.ParseResourceID(strings.TrimRight(clusterResourceID, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse AKS cluster resource ID %q: %w", clusterResourceID, err)
	}
	agentPoolResourceID := clusterID.String() + "/agentPools/" + agentPoolName
	agentPoolID, err := arm.ParseResourceID(agentPoolResourceID)
	if err != nil {
		return nil, fmt.Errorf("parse AKS agent pool resource ID %q: %w", agentPoolResourceID, err)
	}
	machineResourceID := agentPoolID.String() + "/machines/" + machineName
	machineID, err := arm.ParseResourceID(machineResourceID)
	if err != nil {
		return nil, fmt.Errorf("parse AKS machine resource ID %q: %w", machineResourceID, err)
	}
	return machineID, nil
}

func azureClientOptionsFromConfig(cfg *config.Config) azcore.ClientOptions {
	return azclient.ClientOptionsFromConfig(cfg)
}

func getCredential(cfg *config.Config, logger *slog.Logger, clientOpts azcore.ClientOptions) (azcore.TokenCredential, error) {
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
			&azidentity.ClientSecretCredentialOptions{ClientOptions: clientOpts},
		)
	case cfg.IsMIConfigured():
		opts := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: clientOpts}
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
		return azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: clientOpts})
	}
}

func buildK8sProfile(goal GoalState) *armcontainerservice.MachineKubernetesProfile {
	// FlexNode RP accepts the registration surface below; local kubelet defaults
	// are consumed during node bootstrap and must not be sent as Machine fields.
	maxPods := int32(goal.MaxPods) //nolint:gosec // validated non-negative and small
	p := &armcontainerservice.MachineKubernetesProfile{
		OrchestratorVersion: &goal.KubernetesVersion,
		MaxPods:             &maxPods,
		NodeLabels:          stringPointerMap(goal.NodeLabels),
		NodeTaints:          stringPointerSlice(goal.NodeTaints),
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
			// TODO: Read all supported machine goal fields from ARM, not only the
			// Kubernetes version. Otherwise daemon repaves can ignore settings such as
			// max pods, labels, taints, and kubelet config while reporting success.
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
