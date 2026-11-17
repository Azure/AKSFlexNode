package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type startupConvergenceFixture struct {
	events *[]string
	held   bool
}

func (f *startupConvergenceFixture) Converge(context.Context) (bool, error) {
	*f.events = append(*f.events, "units", "reload", "image cleanup")
	return f.held, nil
}

func (f *startupConvergenceFixture) SyncActiveWorker(machine string) error {
	*f.events = append(*f.events, "sync "+machine)
	return nil
}

type startupStateFixture struct {
	events *[]string
	state  *State
	err    error
}

func (s *startupStateFixture) Load(context.Context) (*State, error) {
	*s.events = append(*s.events, "load state")
	return s.state, s.err
}
func (*startupStateFixture) Save(context.Context, *State) error { return nil }
func (*startupStateFixture) Delete(context.Context) error       { return nil }

func TestContainerStartupConvergesBeforeLoadingState(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state   *State
		err     error
		want    []string
		wantErr string
	}{
		"state error still leaves assets converged": {
			err:     errors.New("state unavailable"),
			want:    []string{"units", "reload", "image cleanup", "load state"},
			wantErr: "load daemon state: state unavailable",
		},
		"missing state skips synchronization": {
			want: []string{"units", "reload", "image cleanup", "load state"},
		},
		"invalid worker skips synchronization": {
			state: &State{ActiveMachine: "invalid"},
			want:  []string{"units", "reload", "image cleanup", "load state"},
		},
		"active worker synchronizes after state read": {
			state: &State{ActiveMachine: "kube2"},
			want:  []string{"units", "reload", "image cleanup", "load state", "sync kube2"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var events []string
			store := &startupStateFixture{events: &events, state: tc.state, err: tc.err}
			convergence := &startupConvergenceFixture{events: &events, held: true}
			held, err := convergeContainerStartup(t.Context(), store, convergence)
			if !held {
				t.Fatal("held-assets flag was lost")
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
		})
	}
}
