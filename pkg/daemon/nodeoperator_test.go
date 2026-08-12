package daemon

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestConfigForGoal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goal        aksmachine.GoalState
		wantVersion string
		wantMaxPods int
	}{
		{
			name:        "machine goal overrides startup config",
			goal:        aksmachine.GoalState{KubernetesVersion: "1.36.1", MaxPods: 250},
			wantVersion: "1.36.1",
			wantMaxPods: 250,
		},
		{
			name:        "omitted values preserve startup config",
			goal:        aksmachine.GoalState{},
			wantVersion: "1.35.0",
			wantMaxPods: 30,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			startup := &config.Config{
				Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
				Node:       config.NodeConfig{MaxPods: 30},
			}
			got := (&nspawnNodeOperator{cfg: startup}).configForGoal(tt.goal)

			if got.Components.Kubernetes != tt.wantVersion {
				t.Errorf("Kubernetes version = %q, want %q", got.Components.Kubernetes, tt.wantVersion)
			}
			if got.Node.MaxPods != tt.wantMaxPods {
				t.Errorf("MaxPods = %d, want %d", got.Node.MaxPods, tt.wantMaxPods)
			}
			if got == startup {
				t.Error("configForGoal returned startup config without copying")
			}
			if startup.Components.Kubernetes != "1.35.0" || startup.Node.MaxPods != 30 {
				t.Errorf("startup config was mutated: version=%q, maxPods=%d", startup.Components.Kubernetes, startup.Node.MaxPods)
			}
		})
	}
}

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
