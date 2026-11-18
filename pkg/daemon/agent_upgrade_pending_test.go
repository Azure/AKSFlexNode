package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

type countingStateStore struct {
	state *State
	err   error
	loads int
}

func (s *countingStateStore) Load(context.Context) (*State, error) {
	s.loads++
	return s.state, s.err
}

func (*countingStateStore) Save(context.Context, *State) error { return nil }
func (*countingStateStore) Delete(context.Context) error       { return nil }

func TestRecordPendingReadsSignalBeforeState(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		contents    string
		state       *State
		stateErr    error
		wantErr     string
		wantPending bool
		wantLoads   int
	}{
		"malformed signal": {
			contents:  "{broken",
			stateErr:  errors.New("state unavailable"),
			wantErr:   "decode AgentUpgrade signal",
			wantLoads: 0,
		},
		"same operation": {
			contents:    `{"operationName":"upgrade"}`,
			stateErr:    errors.New("state unavailable"),
			wantPending: true,
			wantLoads:   0,
		},
		"different operation": {
			contents:  `{"operationName":"other"}`,
			stateErr:  errors.New("state unavailable"),
			wantErr:   `another AgentUpgrade operation "other" is pending`,
			wantLoads: 0,
		},
		"state read fails": {
			stateErr:  errors.New("state unavailable"),
			wantErr:   "load daemon state for AgentUpgrade: state unavailable",
			wantLoads: 1,
		},
		"missing state": {
			wantErr:   "no valid active nspawn machine",
			wantLoads: 1,
		},
		"invalid worker": {
			state:     &State{ActiveMachine: "invalid"},
			wantErr:   "no valid active nspawn machine",
			wantLoads: 1,
		},
		"active worker": {
			state:     &State{ActiveMachine: "kube2"},
			wantLoads: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "signal.json")
			if tc.contents != "" {
				if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := &countingStateStore{state: tc.state, err: tc.stateErr}
			signals := agentdeployment.NewUpgradeSignalStore(path)
			err := recordPendingAgentUpgrade(t.Context(), signals, store, "upgrade", "instance")
			if tc.wantPending {
				if !errors.Is(err, errAgentUpgradeAlreadyPending) {
					t.Fatalf("error = %v, want pending sentinel", err)
				}
			} else if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("record pending: %v", err)
			}
			if store.loads != tc.wantLoads {
				t.Fatalf("state loads = %d, want %d", store.loads, tc.wantLoads)
			}
			if tc.contents != "" {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != tc.contents {
					t.Fatalf("existing signal changed: %q, %v", got, err)
				}
			} else if tc.wantErr == "" {
				signal, err := signals.Read()
				if err != nil || signal == nil || signal.ActiveMachine != "kube2" || signal.InitiatingDaemonInstance != "instance" {
					t.Fatalf("recorded signal = %#v, %v", signal, err)
				}
			}
		})
	}
}
