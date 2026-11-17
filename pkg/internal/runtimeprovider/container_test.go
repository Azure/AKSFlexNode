package runtimeprovider_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func mustNodeProvider(t *testing.T, mode isolation.Mode) runtimeprovider.NodeProvider {
	t.Helper()
	p, err := runtimeprovider.New(mode, runtimeprovider.HostServices{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type memoryStateStore struct{ state *runtimeprovider.State }

func (s *memoryStateStore) Load(ctx context.Context) (*runtimeprovider.State, error) {
	return s.state, nil
}
func (s *memoryStateStore) Save(ctx context.Context, state *runtimeprovider.State) error { return nil }
func (s *memoryStateStore) Delete(ctx context.Context) error                             { return nil }

func TestContainerSelectionAndEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                          string
		configured, applied           isolation.Mode
		optIn, containerized, wantErr bool
	}{
		{name: "container opted in", configured: "container", applied: isolation.ModeContainer, optIn: true},
		{name: "container requires opt in", configured: "container", applied: isolation.ModeContainer, wantErr: true},
		{name: "containerized agent refused", configured: "container", applied: isolation.ModeContainer, optIn: true, containerized: true, wantErr: true},
		{name: "container cannot migrate nspawn", configured: "container", applied: isolation.ModeNSpawn, optIn: true, wantErr: true},
		{name: "container cannot migrate none", configured: "container", applied: isolation.ModeNone, optIn: true, wantErr: true},
		{name: "nspawn cannot adopt container", configured: isolation.ModeNSpawn, applied: isolation.ModeContainer, wantErr: true},
		{name: "none cannot adopt container", configured: "none", applied: isolation.ModeContainer, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: tc.configured, ExperimentalContainerIsolation: tc.optIn}}
			appliedCfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeContainer, ExperimentalContainerIsolation: true}}
			state := &runtimeprovider.State{Isolation: tc.applied, ActiveMachine: "kube1", AppliedSettingsVersion: "old", NodeConfig: config.ToAgentConfig(appliedCfg, "kube1").DeepCopy()}
			p := mustNodeProvider(t, tc.configured)
			err := p.ValidateEnvironment(tc.containerized)
			if err == nil {
				_, err = p.NewOperator(t.Context(), cfg, &memoryStateStore{state: state})
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("selection = %v", err)
			}
		})
	}
}

func TestContainerAgentUpgradeRoutingAndCapability(t *testing.T) {
	t.Parallel()
	fixture, err := filepath.Abs("testdata/agent_isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			want := "/var/lib/machines/kube2/usr/local/bin/aks-flex-node"
			if mode == isolation.ModeContainer {
				want = filepath.Join(container.Layout{}.RootfsDir("kube2"), "usr/local/bin/aks-flex-node")
			}
			path := runtimeprovider.NSpawnAgentBinaryPath
			if mode == isolation.ModeContainer {
				path = runtimeprovider.ContainerAgentBinaryPath
			}
			if got := path("kube2"); got != want {
				t.Fatalf("binary sync path = %s, want %s", got, want)
			}
		})
	}
	if err := mustNodeProvider(t, isolation.ModeNone).SynchronizeAgent("missing", ""); err != nil {
		t.Fatal(err)
	}
	if err := mustNodeProvider(t, isolation.ModeContainer).SynchronizeAgent("missing", "../escape"); err == nil {
		t.Fatal("container sync accepted invalid slot")
	}
	for _, tc := range []struct {
		name, supported string
		wantErr         bool
	}{
		{name: "container capable", supported: "container"},
		{name: "none only refused", supported: "none", wantErr: true},
		{name: "old agent refused", supported: "nspawn", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// An immutable script avoids ETXTBUSY from overlapping fixture
			// writes and process forks in parallel tests.
			candidate := filepath.Join(t.TempDir(), tc.supported)
			if err := os.Symlink(fixture, candidate); err != nil {
				t.Fatal(err)
			}
			if err := mustNodeProvider(t, isolation.ModeContainer).CheckAgentCandidate(t.Context(), candidate); (err != nil) != tc.wantErr {
				t.Fatalf("capability = %v", err)
			}
		})
	}
}
