package aksmachine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	testClusterResourceID = "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"
	testAgentPoolName     = "aksflexnodes"
)

func TestMachineResourceIDFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		cfg      *config.Config
		want     string
		wantPool string
		wantErr  string
	}{
		{
			name: "valid config",
			cfg: testARMConfig(
				testClusterResourceID,
				"flex-node-1",
				"1.34.0",
			),
			want:     testClusterResourceID + "/agentPools/aksflexnodes/machines/flex-node-1",
			wantPool: testAgentPoolName,
		},
		{
			name: "trims cluster resource slash",
			cfg: testARMConfig(
				testClusterResourceID+"/",
				"flex-node-1",
				"1.34.0",
			),
			want:     testClusterResourceID + "/agentPools/aksflexnodes/machines/flex-node-1",
			wantPool: testAgentPoolName,
		},
		{
			name: "uses configured agent pool name",
			cfg: func() *config.Config {
				cfg := testARMConfig(
					testClusterResourceID,
					"flex-node-1",
					"1.34.0",
				)
				cfg.Azure.TargetAgentPoolName = "flexnode-edge"
				return cfg
			}(),
			want:     testClusterResourceID + "/agentPools/flexnode-edge/machines/flex-node-1",
			wantPool: "flexnode-edge",
		},
		{
			name: "rejects non cluster resource ID",
			cfg: testARMConfig(
				"/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.Compute/virtualMachines/test-vm",
				"flex-node-1",
				"1.34.0",
			),
			wantErr: "invalid AKS machine resource type",
		},
		{
			name: "missing cluster resource ID",
			cfg: testARMConfig(
				"",
				"flex-node-1",
				"1.34.0",
			),
			wantErr: "incomplete AKS machine config",
		},
		{
			name: "missing node name",
			cfg: testARMConfig(
				testClusterResourceID,
				"",
				"1.34.0",
			),
			wantErr: "incomplete AKS machine config",
		},
		{
			name: "missing agent pool name",
			cfg: func() *config.Config {
				cfg := testARMConfig(
					testClusterResourceID,
					"flex-node-1",
					"1.34.0",
				)
				cfg.Azure.TargetAgentPoolName = ""
				return cfg
			}(),
			wantErr: "incomplete AKS machine config",
		},
		{
			name: "missing Kubernetes version",
			cfg: testARMConfig(
				testClusterResourceID,
				"flex-node-1",
				"",
			),
			wantErr: "incomplete AKS machine config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := machineResourceIDFromConfig(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("machineResourceIDFromConfig() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("machineResourceIDFromConfig() error = %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("machineResourceIDFromConfig() = %q, want %q", got.String(), tt.want)
			}
			if got.Parent == nil || got.Parent.Name != tt.wantPool {
				t.Fatalf("agent pool parent = %#v, want name %q", got.Parent, tt.wantPool)
			}
			if got.Parent.Parent == nil || got.Parent.Parent.Name != "test-cluster" {
				t.Fatalf("cluster parent = %#v, want name test-cluster", got.Parent.Parent)
			}
		})
	}
}

func TestAzureClientOptionsFromConfig(t *testing.T) {
	t.Parallel()

	cfg := testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0")
	cfg.Azure.Cloud = "InvalidCloud"
	cfg.Azure.ResourceManagerEndpointURL = "https://management.example.test"

	opts := azureClientOptionsFromConfig(cfg)
	service, ok := opts.Cloud.Services[cloud.ResourceManager]
	if !ok {
		t.Fatal("ResourceManager cloud service is missing")
	}

	if service.Endpoint != "https://management.example.test" {
		t.Fatalf("ResourceManager endpoint = %q, want https://management.example.test", service.Endpoint)
	}
	if service.Audience != "https://management.example.test" {
		t.Fatalf("ResourceManager audience = %q, want https://management.example.test", service.Audience)
	}
	if opts.Cloud.ActiveDirectoryAuthorityHost != cloud.AzurePublic.ActiveDirectoryAuthorityHost {
		t.Fatalf("authority host = %q, want public cloud", opts.Cloud.ActiveDirectoryAuthorityHost)
	}
}

func TestARMMachineClientOptions(t *testing.T) {
	t.Parallel()

	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("not implemented")
	})
	opts := armMachineClientOptions(azcore.ClientOptions{Transport: transport})

	if opts.Transport == nil {
		t.Fatal("Transport was not preserved")
	}
	if opts.Retry.MaxRetries != armMachineMaxRetries {
		t.Fatalf("MaxRetries = %d, want %d", opts.Retry.MaxRetries, armMachineMaxRetries)
	}
	if opts.Retry.TryTimeout != armMachineTryTimeout {
		t.Fatalf("TryTimeout = %s, want %s", opts.Retry.TryTimeout, armMachineTryTimeout)
	}
	if opts.Retry.RetryDelay != armMachineRetryDelay {
		t.Fatalf("RetryDelay = %s, want %s", opts.Retry.RetryDelay, armMachineRetryDelay)
	}
	if opts.Retry.MaxRetryDelay != armMachineMaxRetryDelay {
		t.Fatalf("MaxRetryDelay = %s, want %s", opts.Retry.MaxRetryDelay, armMachineMaxRetryDelay)
	}
	if opts.Retry.StatusCodes != nil {
		t.Fatalf("StatusCodes = %v, want nil to preserve Azure SDK transient status defaults", opts.Retry.StatusCodes)
	}
}

func TestARMMachineClientRetriesThrottledRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(context.Context, *armMachineClient) error
	}{
		{
			name: "get",
			call: func(ctx context.Context, client *armMachineClient) error {
				_, err := client.Get(ctx)
				return err
			},
		},
		{
			name: "create or update",
			call: func(ctx context.Context, client *armMachineClient) error {
				_, err := client.Create(ctx, GoalState{KubernetesVersion: "1.34.0"})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if attempts.Add(1) == 1 {
					return throttledMachineResponse(request, "1"), nil
				}
				return successfulMachineResponse(t, request), nil
			}))

			if err := tt.call(t.Context(), client); err != nil {
				t.Fatalf("machine request error = %v", err)
			}
			if got := attempts.Load(); got != 2 {
				t.Fatalf("request attempts = %d, want 2", got)
			}
		})
	}
}

func TestARMMachineClientHonorsRetryAfter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		header      string
		headerValue string
	}{
		{name: "seconds", header: "Retry-After", headerValue: "5"},
		{name: "milliseconds", header: "Retry-After-Ms", headerValue: "5000"},
		{name: "ARM milliseconds", header: "x-ms-retry-after-ms", headerValue: "5000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var attempts atomic.Int32
			client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts.Add(1)
				response := throttledMachineResponse(request, "")
				response.Header.Set(tt.header, tt.headerValue)
				return response, nil
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()

			_, err := client.Get(ctx)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Get() error = %v, want context deadline exceeded", err)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("request attempts before %s elapsed = %d, want 1", tt.header, got)
			}
		})
	}
}

func TestARMMachineClientStopsAfterRetryBudget(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	client := newTestARMMachineClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return throttledMachineResponse(request, "1"), nil
	}))

	_, err := client.Get(t.Context())
	var responseErr *azcore.ResponseError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Get() error = %v, want final HTTP 429 response error", err)
	}
	if got, want := attempts.Load(), int32(armMachineMaxRetries+1); got != want {
		t.Fatalf("request attempts = %d, want %d", got, want)
	}
}

func TestClientCertificateCredentialOptionsSendCertificateChain(t *testing.T) {
	t.Parallel()

	clientOptions := azureClientOptionsFromConfig(testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0"))
	options := clientCertificateCredentialOptions(clientOptions)
	if !options.SendCertificateChain {
		t.Fatal("SendCertificateChain = false, want true for Subject Name/Issuer authentication")
	}
	if options.Cloud.ActiveDirectoryAuthorityHost != clientOptions.Cloud.ActiveDirectoryAuthorityHost {
		t.Fatal("ClientOptions were not preserved")
	}
}

func TestGetCredentialClientCertificateLoadError(t *testing.T) {
	t.Parallel()

	cfg := testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0")
	cfg.Azure.ServicePrincipal = &config.ServicePrincipalConfig{
		TenantID:         "tenant",
		ClientID:         "client",
		ClientSecretFile: filepath.Join(t.TempDir(), "missing"),
	}
	credential, err := getCredential(
		cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		azureClientOptionsFromConfig(cfg),
	)
	if err == nil || !strings.Contains(err.Error(), "load service principal client certificate") {
		t.Fatalf("getCredential() error = %v, want client certificate load error", err)
	}
	if credential != nil {
		t.Fatalf("getCredential() = %T, want nil", credential)
	}
}

func TestBuildK8sProfile(t *testing.T) {
	t.Parallel()

	profile := buildK8sProfile(GoalState{
		KubernetesVersion: "1.35.1",
		MaxPods:           42,
		NodeLabels:        map[string]string{"workload": "flex"},
		NodeTaints:        []string{"dedicated=flex:NoSchedule"},
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	})
	if profile.OrchestratorVersion == nil || *profile.OrchestratorVersion != "1.35.1" {
		t.Fatalf("OrchestratorVersion = %v, want 1.35.1", profile.OrchestratorVersion)
	}
	if profile.MaxPods == nil || *profile.MaxPods != 42 {
		t.Fatalf("MaxPods = %v, want 42", profile.MaxPods)
	}
	if got := profile.NodeLabels["workload"]; got == nil || *got != "flex" {
		t.Fatalf("NodeLabels[workload] = %v, want flex", got)
	}
	if len(profile.NodeTaints) != 1 || profile.NodeTaints[0] == nil || *profile.NodeTaints[0] != "dedicated=flex:NoSchedule" {
		t.Fatalf("NodeTaints = %#v, want dedicated=flex:NoSchedule", profile.NodeTaints)
	}
	if profile.KubeletConfig != nil {
		t.Fatalf("KubeletConfig = %#v, want nil because FlexNode RP rejects custom kubelet config on Machine PUT", profile.KubeletConfig)
	}
}

func TestGoalStateValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		goal    GoalState
		wantErr string
	}{
		{
			name: "valid",
			goal: GoalState{KubernetesVersion: "1.35.1"},
		},
		{
			name:    "missing Kubernetes version",
			goal:    GoalState{},
			wantErr: "kubernetes version is empty",
		},
		{
			name:    "negative max pods",
			goal:    GoalState{KubernetesVersion: "1.35.1", MaxPods: -1},
			wantErr: "max pods must be non-negative",
		},
		{
			name:    "max pods exceeds int32",
			goal:    GoalState{KubernetesVersion: "1.35.1", MaxPods: math.MaxInt32 + 1},
			wantErr: "max pods must be less than or equal to",
		},
		{
			name: "negative image GC high threshold",
			goal: GoalState{
				KubernetesVersion: "1.35.1",
				KubeletConfig: KubeletConfig{
					ImageGCHighThreshold: -1,
				},
			},
			wantErr: "image GC high threshold must be non-negative",
		},
		{
			name: "negative image GC low threshold",
			goal: GoalState{
				KubernetesVersion: "1.35.1",
				KubeletConfig: KubeletConfig{
					ImageGCLowThreshold: -1,
				},
			},
			wantErr: "image GC low threshold must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.goal.validate()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}
}

func TestMachineFromARM(t *testing.T) {
	t.Parallel()

	machine := machineFromARM(armcontainerservice.Machine{
		ID:   ptr("machine-id"),
		Name: ptr("node1"),
		Properties: &armcontainerservice.MachineProperties{
			ETag: ptr("settings-42"),
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				OrchestratorVersion: ptr("1.35.1"),
				MaxPods:             ptr(int32(42)),
				NodeLabels:          map[string]*string{"workload": ptr("flex")},
				NodeTaints:          []*string{ptr("dedicated=flex:NoSchedule")},
				KubeletConfig: &armcontainerservice.KubeletConfig{
					ImageGcHighThreshold: ptr(int32(85)),
					ImageGcLowThreshold:  ptr(int32(80)),
				},
			},
			ProvisioningState: ptr("Succeeded"),
		},
	}, "default-id", "default-name")

	if machine.ID != "machine-id" || machine.Name != "node1" {
		t.Fatalf("machine identity = %#v", machine)
	}
	if machine.Goal.KubernetesVersion != "1.35.1" || machine.Goal.SettingsVersion != "settings-42" {
		t.Fatalf("goal versions = %#v", machine.Goal)
	}
	if machine.Goal.MaxPods != 42 || machine.Goal.NodeLabels["workload"] != "flex" {
		t.Fatalf("goal settings = %#v", machine.Goal)
	}
	if len(machine.Goal.NodeTaints) != 1 || machine.Goal.NodeTaints[0] != "dedicated=flex:NoSchedule" {
		t.Fatalf("goal taints = %#v", machine.Goal.NodeTaints)
	}
	if machine.Goal.KubeletConfig.ImageGCHighThreshold != 85 || machine.Goal.KubeletConfig.ImageGCLowThreshold != 80 {
		t.Fatalf("kubelet config = %#v", machine.Goal.KubeletConfig)
	}
	if machine.Status.ProvisioningState != ProvisioningStateSucceeded {
		t.Fatalf("ProvisioningState = %q, want %q", machine.Status.ProvisioningState, ProvisioningStateSucceeded)
	}
}

func TestValidateMachineIdentity(t *testing.T) {
	t.Parallel()

	machineID, err := machineResourceIDFromConfig(testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0"))
	if err != nil {
		t.Fatalf("machineResourceIDFromConfig() error = %v", err)
	}
	client := &armMachineClient{machineID: machineID}

	tests := []struct {
		name    string
		machine armcontainerservice.Machine
		wantErr string
	}{
		{
			name: "matching identity",
			machine: armcontainerservice.Machine{
				ID:   ptr(machineID.String()),
				Name: ptr("flex-node-1"),
			},
		},
		{
			name:    "missing remote identity is allowed",
			machine: armcontainerservice.Machine{},
		},
		{
			name: "ID mismatch",
			machine: armcontainerservice.Machine{
				ID: ptr(testClusterResourceID + "/agentPools/aksflexnodes/machines/other-node"),
			},
			wantErr: "AKS machine ID mismatch",
		},
		{
			name: "name mismatch",
			machine: armcontainerservice.Machine{
				Name: ptr("other-node"),
			},
			wantErr: "AKS machine name mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := client.validateMachineIdentity(tt.machine)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateMachineIdentity() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateMachineIdentity() error = %v", err)
			}
		})
	}
}

func TestMachineFromARMDoesNotUseCurrentOrchestratorVersionAsGoal(t *testing.T) {
	t.Parallel()

	currentVersion := "1.35.2"
	machine := machineFromARM(armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				CurrentOrchestratorVersion: &currentVersion,
			},
		},
	}, "", "")

	if machine.Goal.KubernetesVersion != "" {
		t.Fatalf("KubernetesVersion = %q, want empty without desired orchestratorVersion", machine.Goal.KubernetesVersion)
	}
}

func TestMachineFromARMDoesNotSynthesizeSettingsVersion(t *testing.T) {
	t.Parallel()

	machine := machineFromARM(armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				OrchestratorVersion: ptr("1.35.2"),
			},
		},
	}, "", "")

	if machine.Goal.SettingsVersion != "" {
		t.Fatalf("SettingsVersion = %q, want empty without ETag", machine.Goal.SettingsVersion)
	}
}

func TestMachineFromARMBackfillsIdentity(t *testing.T) {
	t.Parallel()

	machine := machineFromARM(armcontainerservice.Machine{}, "machine-id", "node1")

	if machine.ID != "machine-id" || machine.Name != "node1" {
		t.Fatalf("machine identity = %#v, want backfilled identity", machine)
	}
}

func newTestARMMachineClient(t *testing.T, transport policy.Transporter) *armMachineClient {
	t.Helper()

	machineID, err := machineResourceIDFromConfig(testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0"))
	if err != nil {
		t.Fatalf("machineResourceIDFromConfig() error = %v", err)
	}
	sdkClient, err := armcontainerservice.NewMachinesClient(
		machineID.SubscriptionID,
		staticARMProxyCredential{},
		armMachineClientOptions(azcore.ClientOptions{Transport: transport}),
	)
	if err != nil {
		t.Fatalf("NewMachinesClient() error = %v", err)
	}
	return &armMachineClient{
		machineID: machineID,
		client:    sdkClient,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func throttledMachineResponse(request *http.Request, retryAfterMS string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if retryAfterMS != "" {
		header.Set("Retry-After-Ms", retryAfterMS)
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"SubscriptionRequestsThrottled","message":"try again later"}}`)),
		Request:    request,
	}
}

func successfulMachineResponse(t *testing.T, request *http.Request) *http.Response {
	t.Helper()

	body, err := json.Marshal(armcontainerservice.Machine{
		ID:   ptr(testClusterResourceID + "/agentPools/aksflexnodes/machines/flex-node-1"),
		Name: ptr("flex-node-1"),
		Properties: &armcontainerservice.MachineProperties{
			ETag: ptr("settings-1"),
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				OrchestratorVersion: ptr("1.34.0"),
			},
			ProvisioningState: ptr("Succeeded"),
		},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    request,
	}
}

func testARMConfig(clusterResourceID, nodeName, kubernetesVersion string) *config.Config {
	return &config.Config{
		Azure: config.AzureConfig{
			TargetAgentPoolName: testAgentPoolName,
			TargetCluster: &config.TargetClusterConfig{
				ResourceID: clusterResourceID,
			},
		},
		Agent: config.AgentConfig{
			NodeName: nodeName,
		},
		Components: config.ComponentsConfig{
			Kubernetes: kubernetesVersion,
		},
	}
}

func ptr[T any](v T) *T {
	return &v
}
