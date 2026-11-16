package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

type noneTestStore struct {
	state    *State
	events   *[]string
	failSave bool
}

func (s *noneTestStore) Load(context.Context) (*State, error) { return s.state, nil }
func (s *noneTestStore) Save(_ context.Context, state *State) error {
	*s.events = append(*s.events, "persist")
	if s.failSave {
		return errors.New("disk full")
	}
	s.state = state
	return nil
}
func (s *noneTestStore) Delete(context.Context) error { s.state = nil; return nil }

type fakeNoneNode struct {
	events *[]string
	fail   string
}

func (n fakeNoneNode) call(name string) error {
	*n.events = append(*n.events, name)
	if n.fail == name {
		return errors.New("failed " + name)
	}
	return nil
}
func (n fakeNoneNode) RecoverUpgrade(context.Context) error { return n.call("recover") }
func (n fakeNoneNode) Restart(context.Context) error        { return n.call("restart") }
func (n fakeNoneNode) Upgrade(context.Context) error        { return n.call("upgrade") }
func (n fakeNoneNode) Commit() error                        { return n.call("commit") }
func (n fakeNoneNode) PruneVersions() error                 { return n.call("prune") }
func (n fakeNoneNode) GoalState() (*goalstates.MachineGoalState, error) {
	return &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{}}, nil
}

func TestNoneRepavePersistenceOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, fail string
		failSave   bool
		wantErr    bool
		want       []string
	}{
		{name: "healthy", want: []string{"dependencies", "recover", "artifacts", "npd", "stage", "dependencies", "upgrade", "artifacts", "npd", "persist", "commit", "prune"}},
		{name: "failed dependencies", fail: "dependencies", wantErr: true, want: []string{"dependencies"}},
		{name: "failed recovery", fail: "recover", wantErr: true, want: []string{"dependencies", "recover"}},
		{name: "failed staging", fail: "stage", wantErr: true, want: []string{"dependencies", "recover", "artifacts", "npd", "stage"}},
		{name: "failed upgrade", fail: "upgrade", wantErr: true, want: []string{"dependencies", "recover", "artifacts", "npd", "stage", "dependencies", "upgrade"}},
		{name: "failed persistence keeps journal", failSave: true, wantErr: true, want: []string{"dependencies", "recover", "artifacts", "npd", "stage", "dependencies", "upgrade", "artifacts", "npd", "persist"}},
		{name: "failed commit must not prune rollback artifacts", fail: "commit", want: []string{"dependencies", "recover", "artifacts", "npd", "stage", "dependencies", "upgrade", "artifacts", "npd", "persist", "commit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var events []string
			previous := &agentconfig.AgentConfig{Isolation: "none"}
			previous.Cluster.Version = "1.35.0"
			previous.Kubelet.Labels = map[string]string{noneGenerationLabel: "source-nonce"}
			store := &noneTestStore{events: &events, failSave: tc.failSave, state: &State{Isolation: isolation.ModeNone, NodeConfig: previous, AppliedSettingsVersion: "old", NPDVersion: "v1.35.1"}}
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}, Npd: config.NPDConfig{Version: "v1.36.0"}}
			op := &noneNodeOperator{
				nspawnNodeOperator: &nspawnNodeOperator{cfg: cfg, state: store, bootstrapDataRefresher: &fakeBootstrapDataRefresher{}},
				newNode: func(p noneops.Params) noneNode {
					if p.Previous != nil {
						if p.Previous != previous {
							t.Error("repave replaced the original applied configuration")
						}
						if p.Config.Cluster.Version != "1.36.2" || p.Config.Kubelet.Labels[noneGenerationLabel] == "source-nonce" {
							t.Error("repave lost the target version or fresh nonce")
						}
					} else if p.Config != previous {
						t.Error("recovery replaced the original applied configuration")
					}
					return fakeNoneNode{events: &events, fail: tc.fail}
				},
				resolve: func(_ context.Context, _ *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
					return config.ToAgentConfig(cfg, slot), &goalstates.MachineGoalState{NodeStart: &goalstates.NodeStart{}}, nil, nil
				},
				stage: func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
					return (fakeNoneNode{events: &events, fail: tc.fail}).call("stage")
				},
				dependencies: func(context.Context, *slog.Logger, *config.Config) error {
					return (fakeNoneNode{events: &events, fail: tc.fail}).call("dependencies")
				},
				installNPD: func(context.Context, *slog.Logger, *config.Config, *goalstates.MachineGoalState) error {
					return (fakeNoneNode{events: &events, fail: tc.fail}).call("artifacts")
				},
				startNPD: func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error {
					events = append(events, "npd")
					return nil
				},
			}
			got, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "new", KubernetesVersion: "1.36.2"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("ApplyGoalState = %v", err)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
			if !tc.wantErr {
				if got.Isolation != isolation.ModeNone || got.ActiveMachine != "" || got.NodeConfig.Cluster.Version != "1.36.2" || got.PreviousSettingsVersion != "old" || got.NPDVersion != "v1.36.0" {
					t.Fatalf("incorrect persisted state: mode=%s slot=%s version=%s", got.Isolation, got.ActiveMachine, got.NodeConfig.Cluster.Version)
				}
			}
		})
	}
}

func TestNodeOperatorIsolationSelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, configured string
		applied          isolation.Mode
		missingConfig    bool
		wantErr          bool
	}{
		{name: "default nspawn"},
		{name: "none", configured: "none", applied: isolation.ModeNone},
		{name: "none requires applied configuration", configured: "none", applied: isolation.ModeNone, missingConfig: true, wantErr: true},
		{name: "none cannot adopt nspawn", configured: "none", wantErr: true},
		{name: "nspawn cannot adopt none", applied: isolation.ModeNone, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: tc.configured}}
			state := &State{Isolation: tc.applied}
			if !tc.missingConfig {
				state.NodeConfig = &agentconfig.AgentConfig{Isolation: string(tc.applied)}
			}
			op, err := newNodeOperator(t.Context(), cfg, &testStateStore{state: state})
			if (err != nil) != tc.wantErr {
				t.Fatalf("newNodeOperator = %v", err)
			}
			if tc.wantErr {
				return
			}
			_, isNone := op.(*noneNodeOperator)
			if isNone != (tc.configured == "none") {
				t.Fatal("incorrect node operator")
			}
		})
	}
}

func TestIsolationEnvironment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode             string
		containerized, wantErr bool
	}{
		{name: "host none", mode: "none"},
		{name: "host nspawn", mode: "nspawn"},
		{name: "container nspawn", containerized: true},
		{name: "container none refused", mode: "none", containerized: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: tc.mode}}
			if err := validateIsolationEnvironment(cfg, tc.containerized); (err != nil) != tc.wantErr {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestNoneHostPreparationAndService(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}}
	name := SetupHost(cfg, discardLogger()).Name()
	for _, forbidden := range []string{"harden-apt", "disable-containerd", "disable-kubelet", "install-systemd-container"} {
		if strings.Contains(name, forbidden) {
			t.Fatalf("none setup contains %s: %s", forbidden, name)
		}
	}
	unit, err := renderAgentServiceUnit("/agent", agentServiceOptionsFromConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), "machines.target") {
		t.Fatal("host-native daemon depends on nspawn")
	}
}
