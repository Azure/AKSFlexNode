package daemon

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type orderedWorker struct {
	provider.Provider
	calls                 *[]string
	upgradeErr, retireErr error
	persist               bool
	target                provider.UpgradeParams
}

func (p *orderedWorker) Layout() provider.Layout {
	return provider.Layout{BaseDir: "/unused", KubeletUnit: "kubelet.service"}
}
func (p *orderedWorker) Recover(context.Context, *slog.Logger, provider.StartParams) error {
	*p.calls = append(*p.calls, "recover")
	return nil
}
func (p *orderedWorker) Start(context.Context, *slog.Logger, provider.StartParams) (bool, error) {
	*p.calls = append(*p.calls, "start")
	return p.persist, nil
}
func (p *orderedWorker) Upgrade(_ context.Context, _ *slog.Logger, params provider.UpgradeParams) error {
	*p.calls = append(*p.calls, "upgrade")
	p.target = params
	return p.upgradeErr
}
func (p *orderedWorker) Retire(context.Context, *slog.Logger, provider.UpgradeParams) error {
	*p.calls = append(*p.calls, "retire")
	return p.retireErr
}

type orderedStateStore struct {
	calls   *[]string
	state   *State
	saveErr error
}

func (s *orderedStateStore) Load(context.Context) (*State, error) { return s.state.DeepCopy(), nil }
func (s *orderedStateStore) Save(_ context.Context, state *State) error {
	*s.calls = append(*s.calls, "save")
	if s.saveErr != nil {
		return s.saveErr
	}
	s.state = state.DeepCopy()
	return nil
}
func (s *orderedStateStore) Delete(context.Context) error { s.state = nil; return nil }

func TestWorkerUpgradeSavesBeforeRetirement(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                           string
		upgradeErr, saveErr, retireErr error
		want                           []string
	}{
		{name: "success", want: []string{"recover", "upgrade", "save", "retire"}},
		{name: "upgrade failed", upgradeErr: failure, want: []string{"recover", "upgrade"}},
		{name: "save failed", saveErr: failure, want: []string{"recover", "upgrade", "save"}},
		{name: "retirement failed", retireErr: failure, want: []string{"recover", "upgrade", "save", "retire"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			store := &orderedStateStore{calls: &calls, saveErr: tc.saveErr, state: &State{Isolation: isolation.ModeNSpawn, ActiveMachine: "kube1", AppliedSettingsVersion: "old", AppliedKubernetesVersion: "1.34.0"}}
			runtime := &orderedWorker{calls: &calls, upgradeErr: tc.upgradeErr, retireErr: tc.retireErr}
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeNSpawn}, Agent: config.AgentConfig{NodeName: "node1"}, Azure: config.AzureConfig{TargetAgentPoolName: "flexpool", BootstrapToken: &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"}}, Components: config.ComponentsConfig{Kubernetes: "1.34.0"}, Node: config.NodeConfig{Kubelet: config.KubeletConfig{ClusterFQDN: "api.example.test", CACertData: "Y2E="}}, Networking: config.NetworkingConfig{DNSServiceIP: "10.0.0.10"}}
			op := &workerNodeOperator{cfg: cfg, state: store, runtime: runtime, bootstrapDataRefresher: noopBootstrapDataRefresher{}, lifecycleUnits: phases.Serial(slog.New(slog.DiscardHandler))}
			_, err := op.ApplyGoalState(t.Context(), slog.New(slog.DiscardHandler), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.35.0"})
			wantError := tc.upgradeErr != nil || tc.saveErr != nil || tc.retireErr != nil
			if (err != nil) != wantError {
				t.Fatalf("apply error=%v, want error=%t", err, wantError)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("order=%v, want %v", calls, tc.want)
			}
			if runtime.target.Node.Slot != "kube2" || runtime.target.Previous.Slot != "kube1" || runtime.target.Previous.Config.Cluster.Version != "1.34.0" {
				t.Fatal("incorrect current/target runtime inputs")
			}
			saved := tc.upgradeErr == nil && tc.saveErr == nil
			if saved {
				if store.state.AppliedSettingsVersion != "new" || store.state.PreviousSettingsVersion != "old" || store.state.NodeConfig.Cluster.Version != "1.35.0" {
					t.Fatal("incorrect applied runtime state")
				}
			} else if store.state.AppliedSettingsVersion != "old" {
				t.Fatal("failure changed applied intent")
			}
		})
	}
}

func TestBootstrapRecordsOnlyProvisionedWorker(t *testing.T) {
	t.Parallel()
	for _, persist := range []bool{false, true} {
		name := "running worker"
		if persist {
			name = "provisioned worker"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var calls []string
			runtime := &orderedWorker{calls: &calls, persist: persist}
			store := &orderedStateStore{calls: &calls, state: &State{AppliedSettingsVersion: "old"}}
			task := &startWorkerTask{log: slog.New(slog.DiscardHandler), runtime: runtime, store: store, state: &State{AppliedSettingsVersion: "new"}}
			if err := task.Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := []string{"start"}
			if persist {
				want = append(want, "save")
			}
			if !reflect.DeepEqual(calls, want) {
				t.Fatalf("order=%v, want %v", calls, want)
			}
			if !persist && store.state.AppliedSettingsVersion != "old" {
				t.Fatal("reapply claimed a new applied version")
			}
		})
	}
}
