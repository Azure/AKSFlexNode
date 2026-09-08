package aksmachine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/azclient"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	// ARM can throttle concurrent node bootstrap requests for several minutes.
	// Keep the backoff bounded while allowing a realistic subscription burst to
	// clear without requiring the whole bootstrap workflow to restart.
	armMachineMaxRetries    = 30
	armMachineTryTimeout    = 2 * time.Minute
	armMachineRetryDelay    = 5 * time.Second
	armMachineMaxRetryDelay = time.Minute
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
	armOpts := armMachineClientOptions(clientOpts)
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
	result := machineFromARM(resp.Machine, c.machineID.String(), c.machineID.Name)
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("validate create machine response: %w", err)
	}
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
	result := machineFromARM(resp.Machine, c.machineID.String(), c.machineID.Name)
	if err := result.Validate(); err != nil {
		return nil, fmt.Errorf("validate get machine response: %w", err)
	}
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
		k8sVersion = cfg.Components.Kubernetes
	}
	if clusterResourceID == "" || agentPoolName == "" || machineName == "" || k8sVersion == "" {
		return nil, fmt.Errorf("incomplete AKS machine config: clusterResourceId=%q targetAgentPoolName=%q machineName=%q kubernetesVersion=%q",
			clusterResourceID, agentPoolName, machineName, k8sVersion)
	}
	machineResourceID := strings.TrimRight(clusterResourceID, "/") + "/agentPools/" + agentPoolName + "/machines/" + machineName
	machineID, err := arm.ParseResourceID(machineResourceID)
	if err != nil {
		return nil, fmt.Errorf("parse AKS machine resource ID %q: %w", machineResourceID, err)
	}
	if err := validateMachineResourceID(machineID); err != nil {
		return nil, err
	}
	return machineID, nil
}

func validateMachineResourceID(machineID *arm.ResourceID) error {
	if machineID == nil || machineID.Parent == nil || machineID.Parent.Parent == nil {
		return fmt.Errorf("invalid AKS machine resource ID: missing machine, agent pool, or cluster segment")
	}
	if !strings.EqualFold(machineID.ResourceType.Type, "managedClusters/agentPools/machines") {
		return fmt.Errorf("invalid AKS machine resource type %q, want managedClusters/agentPools/machines", machineID.ResourceType.Type)
	}
	if !strings.EqualFold(machineID.ResourceType.Namespace, "Microsoft.ContainerService") {
		return fmt.Errorf("invalid AKS machine provider %q, want Microsoft.ContainerService", machineID.ResourceType.Namespace)
	}
	return nil
}

func azureClientOptionsFromConfig(cfg *config.Config) azcore.ClientOptions {
	return azclient.ClientOptionsFromConfig(cfg)
}

func armMachineClientOptions(clientOpts azcore.ClientOptions) *arm.ClientOptions {
	// The SDK uses ARM's Retry-After, Retry-After-Ms, and x-ms-retry-after-ms
	// response headers before falling back to this jittered exponential delay.
	clientOpts.Retry = policy.RetryOptions{
		MaxRetries:    armMachineMaxRetries,
		TryTimeout:    armMachineTryTimeout,
		RetryDelay:    armMachineRetryDelay,
		MaxRetryDelay: armMachineMaxRetryDelay,
	}
	return &arm.ClientOptions{ClientOptions: clientOpts}
}

func getCredential(cfg *config.Config, logger *slog.Logger, clientOpts azcore.ClientOptions) (azcore.TokenCredential, error) {
	switch {
	case cfg.IsSPConfigured():
		logger.Debug(
			"using service principal credential for ARM",
			"tenantID", cfg.Azure.ServicePrincipal.TenantID,
			"clientID", cfg.Azure.ServicePrincipal.ClientID,
		)
		if cfg.Azure.ServicePrincipal.ClientSecretFile != "" {
			certificates, privateKey, err := cfg.Azure.ServicePrincipal.LoadClientCertificate()
			if err != nil {
				return nil, fmt.Errorf("load service principal client certificate: %w", err)
			}
			return azidentity.NewClientCertificateCredential(
				cfg.Azure.ServicePrincipal.TenantID,
				cfg.Azure.ServicePrincipal.ClientID,
				certificates,
				privateKey,
				clientCertificateCredentialOptions(clientOpts),
			)
		}
		return azidentity.NewClientSecretCredential(
			cfg.Azure.ServicePrincipal.TenantID,
			cfg.Azure.ServicePrincipal.ClientID,
			cfg.Azure.ServicePrincipal.ClientSecret,
			&azidentity.ClientSecretCredentialOptions{ClientOptions: clientOpts},
		)
	case cfg.IsARCEnabled():
		logger.Debug("using Azure Arc system-assigned managed identity credential for ARM")
		return azidentity.NewManagedIdentityCredential(
			&azidentity.ManagedIdentityCredentialOptions{ClientOptions: clientOpts},
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

func clientCertificateCredentialOptions(clientOpts azcore.ClientOptions) *azidentity.ClientCertificateCredentialOptions {
	return &azidentity.ClientCertificateCredentialOptions{
		ClientOptions:        clientOpts,
		SendCertificateChain: true,
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

func machineFromARM(machine armcontainerservice.Machine, defaultID, defaultName string) *Machine {
	result := &Machine{ID: defaultID, Name: defaultName}
	if machine.ID != nil {
		result.ID = *machine.ID
	}
	if machine.Name != nil {
		result.Name = *machine.Name
	}
	if machine.Properties == nil {
		return result
	}

	properties := machine.Properties
	if properties.Kubernetes != nil {
		kubernetes := properties.Kubernetes
		if kubernetes.OrchestratorVersion != nil {
			result.Goal.KubernetesVersion = *kubernetes.OrchestratorVersion
		}
		if kubernetes.MaxPods != nil {
			result.Goal.MaxPods = int(*kubernetes.MaxPods)
		}
		if kubernetes.NodeLabels != nil {
			result.Goal.NodeLabels = stringMapFromPointers(kubernetes.NodeLabels)
		}
		if kubernetes.NodeTaints != nil {
			result.Goal.NodeTaints = stringSliceFromPointers(kubernetes.NodeTaints)
		}
		if kubernetes.KubeletConfig != nil {
			if kubernetes.KubeletConfig.ImageGcHighThreshold != nil {
				result.Goal.KubeletConfig.ImageGCHighThreshold = int(*kubernetes.KubeletConfig.ImageGcHighThreshold)
			}
			if kubernetes.KubeletConfig.ImageGcLowThreshold != nil {
				result.Goal.KubeletConfig.ImageGCLowThreshold = int(*kubernetes.KubeletConfig.ImageGcLowThreshold)
			}
		}
	}
	if properties.ETag != nil {
		result.Goal.SettingsVersion = *properties.ETag
	}
	if properties.ProvisioningState != nil {
		result.Status.ProvisioningState = ProvisioningState(*properties.ProvisioningState)
	}
	return result
}

func stringMapFromPointers(values map[string]*string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		if value != nil {
			result[key] = *value
		}
	}
	return result
}

func stringSliceFromPointers(values []*string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != nil {
			result = append(result, *value)
		}
	}
	return result
}

func isARMNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}

var _ MachineClient = (*armMachineClient)(nil)
