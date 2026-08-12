package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestFindActiveMachine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state   *State
		want    string
		wantErr bool
	}{
		"kube1": {
			state: &State{ActiveMachine: goalstates.NSpawnMachineKube1},
			want:  goalstates.NSpawnMachineKube1,
		},
		"kube2": {
			state: &State{ActiveMachine: goalstates.NSpawnMachineKube2},
			want:  goalstates.NSpawnMachineKube2,
		},
		"missing state": {
			wantErr: true,
		},
		"missing active machine": {
			state:   &State{},
			wantErr: true,
		},
		"invalid active machine": {
			state:   &State{ActiveMachine: "kube3"},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := (&nspawnNodeOperator{state: &testStateStore{state: tt.state}}).findActiveMachine(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("findActiveMachine error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("findActiveMachine: %v", err)
			}
			if got.Name != tt.want {
				t.Fatalf("machine = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestConfigForGoalStateRefreshesBootstrapData(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Azure: config.AzureConfig{
			BootstrapToken:  &config.BootstrapTokenConfig{Token: "oldtok.0123456789abcdef"},
			ManagedIdentity: &config.ManagedIdentityConfig{ClientID: "identity"},
		},
		Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
		Node: config.NodeConfig{Kubelet: config.KubeletConfig{
			ClusterFQDN: "old.example.test",
			CACertData:  "b2xk",
		}},
	}
	refresher := &fakeBootstrapDataRefresher{data: &bootstrapdata.Data{
		BootstrapToken: "newtok.0123456789abcdef",
		ClusterFQDN:    "new.example.test",
		CACertData:     "bmV3",
	}}
	operator := &nspawnNodeOperator{cfg: cfg, bootstrapDataRefresher: refresher}

	got, err := operator.configForGoalState(t.Context(), discardLogger(), aksmachine.GoalState{KubernetesVersion: "1.36.2"})
	if err != nil {
		t.Fatalf("configForGoalState() error = %v", err)
	}
	if refresher.calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refresher.calls)
	}
	if got.Azure.BootstrapToken.Token != "newtok.0123456789abcdef" {
		t.Fatalf("bootstrap token was not refreshed")
	}
	if got.Node.Kubelet.ClusterFQDN != "new.example.test" || got.Node.Kubelet.CACertData != "bmV3" {
		t.Fatalf("kubelet bootstrap data = %#v", got.Node.Kubelet)
	}
	if got.Components.Kubernetes != "1.36.2" {
		t.Fatalf("Kubernetes version = %q", got.Components.Kubernetes)
	}
	if cfg.Azure.BootstrapToken.Token != "oldtok.0123456789abcdef" {
		t.Fatal("original config bootstrap token was mutated")
	}
}

func TestConfigForGoalStateSkipsBootstrapDataRefreshWithoutBothAuthTypes(t *testing.T) {
	t.Parallel()

	tests := map[string]*config.Config{
		"token only": {
			Azure: config.AzureConfig{BootstrapToken: &config.BootstrapTokenConfig{Token: "oldtok.0123456789abcdef"}},
		},
		"managed identity only": {
			Azure: config.AzureConfig{ManagedIdentity: &config.ManagedIdentityConfig{}},
		},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refresher := &fakeBootstrapDataRefresher{}
			operator := &nspawnNodeOperator{cfg: cfg, bootstrapDataRefresher: refresher}
			if _, err := operator.configForGoalState(t.Context(), discardLogger(), aksmachine.GoalState{}); err != nil {
				t.Fatalf("configForGoalState() error = %v", err)
			}
			if refresher.calls != 0 {
				t.Fatalf("refresh calls = %d, want 0", refresher.calls)
			}
		})
	}
}

func TestConfigForGoalStateBootstrapDataRefreshFailure(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Azure: config.AzureConfig{
		BootstrapToken:  &config.BootstrapTokenConfig{Token: "oldtok.0123456789abcdef"},
		ManagedIdentity: &config.ManagedIdentityConfig{},
	}}
	operator := &nspawnNodeOperator{
		cfg:                    cfg,
		bootstrapDataRefresher: &fakeBootstrapDataRefresher{err: errors.New("ARM unavailable")},
	}
	_, err := operator.configForGoalState(t.Context(), discardLogger(), aksmachine.GoalState{})
	if err == nil || !errors.Is(err, operator.bootstrapDataRefresher.(*fakeBootstrapDataRefresher).err) {
		t.Fatalf("configForGoalState() error = %v", err)
	}
	if cfg.Azure.BootstrapToken.Token != "oldtok.0123456789abcdef" {
		t.Fatal("original config bootstrap token was mutated")
	}
}

func TestBootstrapDataOptionsFromConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Azure: config.AzureConfig{
		ResourceManagerEndpointURL: "https://management.usgovcloudapi.net",
		ManagedIdentity:            &config.ManagedIdentityConfig{ClientID: "identity"},
		TargetCluster: &config.TargetClusterConfig{
			ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		},
		TargetAgentPoolName: "pool",
	}}
	got, err := bootstrapDataOptionsFromConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapDataOptionsFromConfig() error = %v", err)
	}
	if got.AuthMode != "managed-identity" || got.MSIClientID != "identity" {
		t.Fatalf("managed identity options = %#v", got)
	}
	if got.ResourceManagerEndpoint != "https://management.usgovcloudapi.net" || got.AuthorityHost != "https://login.microsoftonline.us/" {
		t.Fatalf("sovereign cloud options = %#v", got)
	}
	if got.APIVersion != bootstrapdata.DefaultAPIVersion {
		t.Fatalf("API version = %q", got.APIVersion)
	}
}

func TestBootstrapDataOptionsFromServicePrincipal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		servicePrincipal config.ServicePrincipalConfig
		wantSecret       string
		wantCertificate  string
	}{
		"inline secret": {
			servicePrincipal: config.ServicePrincipalConfig{TenantID: "tenant", ClientID: "client", ClientSecret: "secret"},
			wantSecret:       "secret",
		},
		"certificate file": {
			servicePrincipal: config.ServicePrincipalConfig{TenantID: "tenant", ClientID: "client", ClientSecretFile: "/credentials/client.pem"},
			wantCertificate:  "/credentials/client.pem",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Azure: config.AzureConfig{
				ServicePrincipal: &tt.servicePrincipal,
				TargetCluster: &config.TargetClusterConfig{
					ResourceID: "/subscriptions/sub/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
				},
				TargetAgentPoolName: "pool",
			}}
			got, err := bootstrapDataOptionsFromConfig(cfg)
			if err != nil {
				t.Fatalf("bootstrapDataOptionsFromConfig() error = %v", err)
			}
			if got.AuthMode != "service-principal" || got.SPTenantID != "tenant" || got.SPClientID != "client" {
				t.Fatalf("service-principal identity options = %#v", got)
			}
			if got.SPClientSecret != tt.wantSecret || got.SPClientCertificateFile != tt.wantCertificate {
				t.Fatalf("service-principal credential options = %#v", got)
			}
		})
	}
}

type fakeBootstrapDataRefresher struct {
	data  *bootstrapdata.Data
	err   error
	calls int
}

func (f *fakeBootstrapDataRefresher) Fetch(context.Context, *config.Config) (*bootstrapdata.Data, error) {
	f.calls++
	return f.data, f.err
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testStateStore struct {
	state *State
}

func (s *testStateStore) Load(context.Context) (*State, error) {
	return s.state, nil
}

func (s *testStateStore) Save(context.Context, *State) error {
	return nil
}

func (s *testStateStore) Delete(context.Context) error {
	return nil
}
