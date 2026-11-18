package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestValidateStartupState(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		applied *State
		wantErr string
	}{
		{
			name: "nspawn without state",
			mode: isolation.ModeNSpawn,
		},
		{
			name:    "none without state",
			mode:    isolation.ModeNone,
			wantErr: "applied none worker state is missing",
		},
		{
			name:    "OCI without state",
			mode:    isolation.ModeContainer,
			wantErr: "applied container worker state is missing",
		},
		{
			name:    "none with state",
			mode:    isolation.ModeNone,
			applied: &State{Isolation: isolation.ModeNone},
		},
		{
			name:    "OCI with state",
			mode:    isolation.ModeContainer,
			applied: &State{Isolation: isolation.ModeContainer},
		},
		{
			name:    "mode change",
			mode:    isolation.ModeContainer,
			applied: &State{Isolation: isolation.ModeNone},
			wantErr: "worker isolation change requires reset",
		},
		{
			name:    "empty mode",
			wantErr: "unsupported worker isolation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateStartupState(tc.mode, tc.applied)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validation error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

type startupRecoveryRuntime struct {
	fakeWorkerRuntime
	input agentruntime.Inputs
	calls int
	err   error
}

func (r *startupRecoveryRuntime) Recover(_ *slog.Logger, input agentruntime.Inputs) phases.Task {
	r.calls++
	r.input = input
	return testTask{
		name: "recover",
		run:  func() error { return r.err },
	}
}

func TestRecoverStartupWorkerUsesAppliedState(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Bootstrap: config.BootstrapConfig{
					Isolation: mode,
				},
			}
			appliedConfig := &agentconfig.AgentConfig{}
			applied := &State{ActiveMachine: "kube2", NodeConfig: appliedConfig}
			worker := &startupRecoveryRuntime{}
			if err := recoverStartupWorker(t.Context(), cfg, discardLogger(), worker, applied); err != nil {
				t.Fatal(err)
			}
			if mode == isolation.ModeNSpawn {
				if worker.calls != 0 {
					t.Fatal("nspawn startup unexpectedly recovered a worker")
				}
				return
			}
			if worker.calls != 1 || worker.input.Config != cfg || worker.input.Node.Slot != "kube2" || worker.input.Node.Config != appliedConfig {
				t.Fatalf("recovery input = %+v, calls = %d", worker.input, worker.calls)
			}
		})
	}
}

func TestRecoverStartupWorkerPropagatesFailure(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Bootstrap: config.BootstrapConfig{
			Isolation: isolation.ModeNone,
		},
	}
	worker := &startupRecoveryRuntime{err: errors.New("recovery failed")}
	err := recoverStartupWorker(t.Context(), cfg, discardLogger(), worker, &State{})
	if err == nil || !strings.Contains(err.Error(), "recover worker at startup: recovery failed") {
		t.Fatalf("recovery error = %v", err)
	}
}

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
