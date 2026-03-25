package v20260301

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/aksmachine"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

const (
	aksFlexNodePoolName = "aksflexnodes"
	// flexNodeTagKey is the tag that identifies this machine as an AKS flex node.
	flexNodeTagKey = "aks-flex-node"

	// ARM calls to a local test server. It is for testing only and should not be set in production.
	armEndpointOverride = ""
)

type ensureMachineAction struct {
	logger *logrus.Logger
}

func newEnsureMachineAction() (actions.Server, error) {
	return &ensureMachineAction{
		logger: logrus.New(),
	}, nil
}

var _ actions.Server = (*ensureMachineAction)(nil)

// ApplyAction runs two sequential sub-steps:
//  1. Ensure the "aksflexnodes" agent pool exists with mode "Machines".
//  2. Ensure the local machine exists in that pool tagged as a flex node.
//
// If drift detection and remediation is not enabled in the agent config, the
// action returns immediately without performing any Azure operations.
func (a *ensureMachineAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	action, err := utilpb.AnyTo[*aksmachine.EnsureMachine](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec := action.GetSpec()

	// Skip all Azure operations when drift detection/remediation is disabled.
	if !spec.GetEnabled() {
		a.logger.Info("EnsureMachine: drift detection and remediation is disabled, skipping")
		item, err := anypb.New(action)
		if err != nil {
			return nil, err
		}
		return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
	}

	subID := spec.GetSubscriptionId()
	rg := spec.GetResourceGroup()
	clusterName := spec.GetClusterName()
	machineName := spec.GetMachineName()
	k8sVersion := spec.GetKubernetesVersion()

	if subID == "" || rg == "" || clusterName == "" || machineName == "" || k8sVersion == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"EnsureMachine: spec fields incomplete: subscriptionId=%q resourceGroup=%q clusterName=%q machineName=%q kubernetesVersion=%q",
			subID, rg, clusterName, machineName, k8sVersion)
	}

	cred, err := credentialFromSpec(spec.GetAzureCredential())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "EnsureMachine: resolve credential: %v", err)
	}

	armOpts := buildARMClientOptions(armEndpointOverride)

	// Step 1: ensure the agent pool exists with mode "Machines".
	if err := a.ensureAgentPool(ctx, cred, armOpts, subID, rg, clusterName); err != nil {
		return nil, status.Errorf(codes.Internal, "EnsureMachine: ensure agent pool: %v", err)
	}

	// Step 2: ensure this machine is registered in the pool as a flex node.
	if err := a.ensureMachine(ctx, cred, armOpts, spec); err != nil {
		return nil, status.Errorf(codes.Internal, "EnsureMachine: ensure machine: %v", err)
	}

	item, err := anypb.New(action)
	if err != nil {
		return nil, err
	}
	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensureAgentPool calls CreateOrUpdate on the "aksflexnodes" agent pool with
// mode "Machines" and waits for the long-running operation to complete.
func (a *ensureMachineAction) ensureAgentPool(ctx context.Context, cred azcore.TokenCredential, armOpts *arm.ClientOptions, subID, rg, clusterName string) error {
	client, err := armcontainerservice.NewAgentPoolsClient(subID, cred, armOpts)
	if err != nil {
		return fmt.Errorf("create agent pools client: %w", err)
	}

	mode := armcontainerservice.AgentPoolMode("Machines")
	params := armcontainerservice.AgentPool{
		Properties: &armcontainerservice.ManagedClusterAgentPoolProfileProperties{
			Mode: &mode,
		},
	}

	a.logger.Infof("Ensuring agent pool %q (mode=Machines) on cluster %s/%s", aksFlexNodePoolName, rg, clusterName)

	// Check whether the agent pool already exists; if so, skip the PUT.
	_, err = client.Get(ctx, rg, clusterName, aksFlexNodePoolName, nil)
	if err == nil {
		a.logger.Infof("Agent pool %q already exists on cluster %s/%s, skipping", aksFlexNodePoolName, rg, clusterName)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get agent pool %q: %w", aksFlexNodePoolName, err)
	}

	poller, err := client.BeginCreateOrUpdate(ctx, rg, clusterName, aksFlexNodePoolName, params, nil)
	if err != nil {
		return fmt.Errorf("begin create or update agent pool %q: %w", aksFlexNodePoolName, err)
	}

	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for agent pool %q: %w", aksFlexNodePoolName, err)
	}

	a.logger.Infof("Agent pool %q ensured on cluster %s/%s", aksFlexNodePoolName, rg, clusterName)
	return nil
}

// ensureMachine registers this machine in the "aksflexnodes" agent pool as a
// flex node. It first checks whether the machine resource already exists; if so
// it skips the PUT to avoid overwriting properties managed by the AKS control plane.
func (a *ensureMachineAction) ensureMachine(ctx context.Context, cred azcore.TokenCredential, armOpts *arm.ClientOptions, spec *aksmachine.EnsureMachineSpec) error {
	subID := spec.GetSubscriptionId()
	rg := spec.GetResourceGroup()
	clusterName := spec.GetClusterName()
	machineName := spec.GetMachineName()

	client, err := armcontainerservice.NewMachinesClient(subID, cred, armOpts)
	if err != nil {
		return fmt.Errorf("create machines client: %w", err)
	}

	// Check whether the machine is already registered; if so, skip the PUT.
	_, err = client.Get(ctx, rg, clusterName, aksFlexNodePoolName, machineName, nil)
	if err == nil {
		a.logger.Infof("Machine %q already exists in pool %q on cluster %s/%s, skipping", machineName, aksFlexNodePoolName, rg, clusterName)
		return nil
	}
	if !isNotFound(err) {
		return fmt.Errorf("get machine %q: %w", machineName, err)
	}

	params := armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Tags: map[string]*string{
				flexNodeTagKey: to.Ptr("true"),
			},
			Kubernetes: buildK8sProfile(spec),
		},
	}

	poller, err := client.BeginCreateOrUpdate(ctx, rg, clusterName, aksFlexNodePoolName, machineName, params, nil)
	if err != nil {
		return fmt.Errorf("begin create or update machine %q: %w", machineName, err)
	}

	// if the ARM server returns a synchronous 2xx response
	// with no Azure-AsyncOperation / Operation-Location / Location header, the SDK treats it as synchronously
	// complete and PollUntilDone returns right away with the response body — no looping occurs.
	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for machine %q: %w", machineName, err)
	}

	a.logger.Infof("Machine %q ensured in pool %q on cluster %s/%s", machineName, aksFlexNodePoolName, rg, clusterName)
	return nil
}

// buildK8sProfile constructs a MachineKubernetesProfile from the spec using
// the explicit allow-list of fields permitted for flex nodes:
//   - OrchestratorVersion, MaxPods, NodeLabels, NodeTaints,
//     NodeInitializationTaints, KubeletConfig (image GC thresholds).
func buildK8sProfile(spec *aksmachine.EnsureMachineSpec) *armcontainerservice.MachineKubernetesProfile {
	p := &armcontainerservice.MachineKubernetesProfile{}

	if v := spec.GetKubernetesVersion(); v != "" {
		p.OrchestratorVersion = to.Ptr(v)
	}
	if mp := spec.GetMaxPods(); mp > 0 {
		p.MaxPods = to.Ptr(mp)
	}
	if labels := spec.GetNodeLabels(); len(labels) > 0 {
		p.NodeLabels = make(map[string]*string, len(labels))
		for k, v := range labels {
			p.NodeLabels[k] = to.Ptr(v)
		}
	}
	if taints := spec.GetNodeTaints(); len(taints) > 0 {
		p.NodeTaints = make([]*string, len(taints))
		for i, t := range taints {
			p.NodeTaints[i] = to.Ptr(t)
		}
	}
	if initTaints := spec.GetNodeInitializationTaints(); len(initTaints) > 0 {
		p.NodeInitializationTaints = make([]*string, len(initTaints))
		for i, t := range initTaints {
			p.NodeInitializationTaints[i] = to.Ptr(t)
		}
	}
	if kc := spec.GetKubeletConfig(); kc != nil {
		p.KubeletConfig = &armcontainerservice.KubeletConfig{}
		if h := kc.GetImageGcHighThreshold(); h > 0 {
			p.KubeletConfig.ImageGcHighThreshold = to.Ptr(h)
		}
		if l := kc.GetImageGcLowThreshold(); l > 0 {
			p.KubeletConfig.ImageGcLowThreshold = to.Ptr(l)
		}
	}

	return p
}

// credentialFromSpec resolves an Azure ARM credential from the proto AzureCredential field.
// Falls back to Azure CLI credential when the field is absent or empty.
func credentialFromSpec(cred *aksmachine.AzureCredential) (azcore.TokenCredential, error) {
	if sp := cred.GetServicePrincipal(); sp != nil {
		return azidentity.NewClientSecretCredential(sp.GetTenantId(), sp.GetClientId(), sp.GetClientSecret(), nil)
	}
	if mi := cred.GetManagedIdentity(); mi != nil {
		opts := &azidentity.ManagedIdentityCredentialOptions{}
		if id := mi.GetClientId(); id != "" {
			opts.ID = azidentity.ClientID(id)
		}
		return azidentity.NewManagedIdentityCredential(opts)
	}
	// return azidentity.NewAzureCLICredential(nil)
	return nil, nil
}

// buildARMClientOptions returns ARM client options that redirect all calls to
// endpointOverride when non-empty (e.g. "http://localhost:8080" for local testing).
// Returns nil when the override is empty, which causes the SDK to use the default
// public Azure Resource Manager endpoint.
func buildARMClientOptions(endpointOverride string) *arm.ClientOptions {
	if endpointOverride == "" {
		return nil
	}
	return &arm.ClientOptions{
		ClientOptions: azcore.ClientOptions{
			Cloud: cloud.Configuration{
				Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
					cloud.ResourceManager: {
						Endpoint: endpointOverride,
						// No audience needed for local servers that don't validate tokens.
						Audience: endpointOverride,
					},
				},
			},
			InsecureAllowCredentialWithHTTP: true,
		},
	}
}

// isNotFound reports whether the Azure SDK error is an HTTP 404.
func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}
