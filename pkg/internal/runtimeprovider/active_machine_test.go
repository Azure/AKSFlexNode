package runtimeprovider

import (
	"testing"
)

func TestActiveMachineFromStore(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state   *State
		want    string
		wantErr bool
	}{
		"kube1": {
			state: &State{ActiveMachine: "kube1"},
			want:  "kube1",
		},
		"kube2": {
			state: &State{ActiveMachine: "kube2"},
			want:  "kube2",
		},
		"missing state": {
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

			got, err := activeMachineFromStore(t.Context(), &testStateStore{state: tt.state})
			if tt.wantErr {
				if err == nil {
					t.Fatal("activeMachineFromStore error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("activeMachineFromStore: %v", err)
			}
			if got.Name != tt.want {
				t.Fatalf("machine = %q, want %q", got.Name, tt.want)
			}
		})
	}
}
