package daemon

import (
	"context"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestActiveMachineFromState(t *testing.T) {
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
