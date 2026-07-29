package daemon

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
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
			state: &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: goalstates.NSpawnMachineKube1},
			want:  goalstates.NSpawnMachineKube1,
		},
		"kube2": {
			state: &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: goalstates.NSpawnMachineKube2},
			want:  goalstates.NSpawnMachineKube2,
		},
		"missing state": {
			wantErr: true,
		},
		"missing active machine": {
			state:   &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}},
			wantErr: true,
		},
		"missing applied goal": {
			state:   &State{ActiveMachine: goalstates.NSpawnMachineKube1},
			wantErr: true,
		},
		"invalid active machine": {
			state:   &State{AppliedGoal: &aksmachine.GoalState{KubernetesVersion: "1.34.0"}, ActiveMachine: "kube3"},
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

func TestAcknowledgeGoalState(t *testing.T) {
	t.Parallel()

	store := &testStateStore{state: &State{
		AppliedGoal: &aksmachine.GoalState{
			KubernetesVersion: "1.34.0",
			SettingsVersion:   "41",
		},
		ActiveMachine: goalstates.NSpawnMachineKube1,
	}}
	operator := &nspawnNodeOperator{state: store}
	goal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		SettingsVersion:   "42",
		NodeLabels:        map[string]string{"workload": "flex"},
		NodeTaints:        []string{"dedicated=flex:NoSchedule"},
	}

	got, err := operator.AcknowledgeGoalState(t.Context(), goal)
	if err != nil {
		t.Fatalf("AcknowledgeGoalState: %v", err)
	}
	if got.AppliedGoal == nil || got.AppliedGoal.SettingsVersion != "42" || got.ActiveMachine != goalstates.NSpawnMachineKube1 {
		t.Fatalf("state = %#v", got)
	}
	if got.PreviousAppliedGoal == nil || got.PreviousAppliedGoal.SettingsVersion != "41" {
		t.Fatalf("PreviousAppliedGoal = %#v, want settings version 41", got.PreviousAppliedGoal)
	}
	if got.AppliedGoal == nil || got.AppliedGoal.NodeLabels["workload"] != "flex" || len(got.AppliedGoal.NodeTaints) != 1 {
		t.Fatalf("AppliedGoal = %#v", got.AppliedGoal)
	}
	if store.state != got {
		t.Fatal("acknowledged state was not persisted")
	}
}

type testStateStore struct {
	state *State
}

func (s *testStateStore) Load(context.Context) (*State, error) {
	return s.state, nil
}

func (s *testStateStore) Save(_ context.Context, state *State) error {
	s.state = state
	return nil
}

func (s *testStateStore) Delete(context.Context) error {
	return nil
}
