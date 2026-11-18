package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type recordingRuntime struct {
	fakeWorkerRuntime
	events  *[]string
	fail    string
	machine string
}

func (r *recordingRuntime) task(name string) phases.Task {
	return testTask{
		name: name,
		run: func() error {
			*r.events = append(*r.events, name)
			if name == r.fail {
				return errors.New(name + " failed")
			}
			return nil
		},
	}
}

func (r *recordingRuntime) Stop(_ *slog.Logger, machine string) phases.Task {
	r.machine = machine
	return r.task("stop")
}

func (r *recordingRuntime) CleanupNetwork(*slog.Logger) phases.Task {
	return r.task("network")
}

func (r *recordingRuntime) Start(_ *config.Config, _ *slog.Logger, machine string, _ *goalstates.MachineGoalState, _ *goalstates.ContainerImageArchiveStaging) phases.Task {
	return testTask{
		name: "start",
		run: func() error {
			if machine != goalstates.NSpawnMachineKube2 {
				return errors.New("wrong replacement machine")
			}
			return r.task("start").Do(context.Background())
		},
	}
}

func (r *recordingRuntime) Retire(_ *slog.Logger, machine string) phases.Task {
	return testTask{
		name: "retire",
		run: func() error {
			if machine != r.machine {
				return errors.New("wrong retired machine")
			}
			return r.task("retire").Do(context.Background())
		},
	}
}

type recordingStateStore struct {
	state  *State
	events *[]string
	fail   bool
}

func (s *recordingStateStore) Load(context.Context) (*State, error) {
	return s.state, nil
}

func (s *recordingStateStore) Delete(context.Context) error {
	s.state = nil
	return nil
}

func (s *recordingStateStore) Save(_ context.Context, state *State) error {
	*s.events = append(*s.events, "save")
	if s.fail {
		return errors.New("save failed")
	}
	s.state = state
	return nil
}

func TestRepaveExecutionOrderingAndFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		fail     string
		want     []string
		wantSave bool
	}{
		{
			name: "success",
			want: []string{
				"stop",
				"network",
				"hooks",
				"start",
				"save",
				"retire",
			},
			wantSave: true,
		},
		{
			name: "stop failure",
			fail: "stop",
			want: []string{
				"stop",
			},
		},
		{
			name: "network failure",
			fail: "network",
			want: []string{
				"stop",
				"network",
			},
		},
		{
			name: "hook failure",
			fail: "hooks",
			want: []string{
				"stop",
				"network",
				"hooks",
			},
		},
		{
			name: "start failure",
			fail: "start",
			want: []string{
				"stop",
				"network",
				"hooks",
				"start",
			},
		},
		{
			name: "save failure",
			fail: "save",
			want: []string{
				"stop",
				"network",
				"hooks",
				"start",
				"save",
			},
		},
		{
			name: "retire failure",
			fail: "retire",
			want: []string{
				"stop",
				"network",
				"hooks",
				"start",
				"save",
				"retire",
			},
			wantSave: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var events []string
			worker := &recordingRuntime{events: &events, fail: tc.fail}
			store := &recordingStateStore{
				state: &State{
					ActiveMachine:          goalstates.NSpawnMachineKube1,
					AppliedSettingsVersion: "old",
				},
				events: &events,
				fail:   tc.fail == "save",
			}
			r := newTestRepaveReconciler(t, &fakeMachineClient{}, fakeClient(), &fakeWorkerRuntime{})
			r.runtime = worker
			r.state = store
			r.hookUnits = func(*slog.Logger) phases.Task { return worker.task("hooks") }

			_, err := r.executeGoalState(t.Context(), aksmachine.GoalState{SettingsVersion: "new"})
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("executeGoalState error = %v", err)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
			if got := store.state.AppliedSettingsVersion == "new"; got != tc.wantSave {
				t.Fatalf("state saved = %v, want %v", got, tc.wantSave)
			}
		})
	}
}

func TestAgentResetPublishesBeforeStoppingDaemon(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		finishErr  error
		cleanupErr error
		wantStop   bool
	}{
		{
			name:     "published",
			wantStop: true,
		},
		{
			name:      "publish failed",
			finishErr: errors.New("publish failed"),
		},
		{
			name:       "configuration cleanup failed",
			cleanupErr: errors.New("cleanup failed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			worker := &fakeWorkerRuntime{}
			store := &fakeMachineOperationStore{finishErr: tc.finishErr}
			stopped := false
			h := &machineOperationHandlers{
				log:     slog.Default(),
				runtime: worker,
				clearConfig: testTask{
					name: "remove-runtime-dirs",
					run:  func() error { return tc.cleanupErr },
				},
				uninstall: func(*slog.Logger) phases.Task {
					return testTask{
						name: "uninstall",
						run: func() error {
							if store.result.Phase != machinav1alpha3.OperationPhaseComplete {
								t.Fatal("daemon stopped before reset completion was published")
							}
							stopped = true
							return nil
						},
					}
				},
			}
			_, err := h.reconcileAgentReset(t.Context(), store, daemon.MachineOperation{Name: "reset"})
			if (err != nil) != (tc.finishErr != nil) {
				t.Fatalf("reconcileAgentReset error = %v", err)
			}
			if tc.cleanupErr != nil && store.result.Phase != machinav1alpha3.OperationPhaseFailed {
				t.Fatalf("phase = %s, want failure", store.result.Phase)
			}
			if stopped != tc.wantStop {
				t.Fatalf("stopped = %v, want %v", stopped, tc.wantStop)
			}
		})
	}
}
