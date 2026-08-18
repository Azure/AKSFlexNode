package aksmachine

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestEnsureMachineCreateFailure(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		require bool
		wantErr string
	}{
		"best effort ignores create failure": {},
		"required returns create failure": {
			require: true,
			wantErr: "ensure-machine: create machine: boom",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := &ensureMachineClient{createErr: errors.New("boom")}
			goal := testGoal("1.35.1", "")
			task := EnsureMachine(client, &goal, tt.require, slog.New(slog.NewTextHandler(io.Discard, nil)))

			err := task.Do(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Do() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
		})
	}
}

func TestEnsureMachineGetFailure(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		require bool
		wantErr string
	}{
		"best effort ignores get failure": {},
		"required returns get failure": {
			require: true,
			wantErr: "ensure-machine: get machine: boom",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := &ensureMachineClient{getErr: errors.New("boom")}
			goal := testGoal("1.35.1", "")
			task := EnsureMachine(client, &goal, tt.require, slog.New(slog.NewTextHandler(io.Discard, nil)))

			err := task.Do(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Do() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
		})
	}
}

func TestEnsureMachineCreatesAndAdoptsSettingsVersion(t *testing.T) {
	t.Parallel()

	goal := testGoal("1.35.1", "")
	createdGoal := testMachineGoal("1.35.1", "etag-created")
	createdGoal.MaxPods = ptr(42)
	client := &ensureMachineClient{createResult: &Machine{Goal: createdGoal}}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if client.createCalls != 1 {
		t.Fatalf("Create() calls = %d, want 1", client.createCalls)
	}
	if goal.SettingsVersion != "etag-created" {
		t.Fatalf("SettingsVersion = %q, want etag-created", goal.SettingsVersion)
	}
	if goal.MaxPods != 42 {
		t.Fatalf("MaxPods = %d, want server-normalized value 42", goal.MaxPods)
	}
}

func TestEnsureMachineAdoptsExistingGoal(t *testing.T) {
	t.Parallel()

	goal := GoalState{
		KubernetesVersion: "1.35.1",
		MaxPods:           30,
		NodeLabels:        map[string]string{"source": "local"},
		NodeTaints:        []string{"local=true:NoSchedule"},
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
	client := &ensureMachineClient{machine: &Machine{Goal: MachineGoal{
		KubernetesVersion: "1.35.1",
		SettingsVersion:   "etag-42",
		MaxPods:           ptr(110),
		NodeLabels:        map[string]string{"source": "remote"},
		NodeTaints:        []string{"remote=true:NoSchedule"},
		KubeletConfig: MachineKubeletConfig{
			ImageGCHighThreshold: ptr(70),
			ImageGCLowThreshold:  ptr(60),
		},
	}}}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Create() calls = %d, want 0", client.createCalls)
	}
	if goal.SettingsVersion != "etag-42" {
		t.Fatalf("SettingsVersion = %q, want etag-42", goal.SettingsVersion)
	}
	if goal.MaxPods != 110 || goal.NodeLabels["source"] != "remote" || goal.NodeTaints[0] != "remote=true:NoSchedule" {
		t.Fatalf("remote goal was not adopted: %#v", goal)
	}
	if goal.KubeletConfig.ImageGCHighThreshold != 70 || goal.KubeletConfig.ImageGCLowThreshold != 60 {
		t.Fatalf("remote kubelet config was not adopted: %#v", goal.KubeletConfig)
	}
}

func TestEnsureMachineAdoptsExistingMismatchedVersion(t *testing.T) {
	t.Parallel()

	goal := testGoal("1.35.1", "")
	remoteGoal := testMachineGoal("1.34.0", "etag-remote")
	client := &ensureMachineClient{machine: &Machine{Goal: remoteGoal}}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Create() calls = %d, want 0", client.createCalls)
	}
	if goal.KubernetesVersion != "1.34.0" || goal.SettingsVersion != "etag-remote" {
		t.Fatalf("goal = %#v, want remote version and settings version", goal)
	}
}

func TestEnsureMachineRejectsInvalidExistingMachine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		machine *Machine
		require bool
		wantErr string
	}{
		"best effort preserves local goal after missing settings version": {
			machine: &Machine{Goal: testMachineGoal("1.35.1", "")},
		},
		"required rejects missing settings version": {
			machine: &Machine{Goal: testMachineGoal("1.35.1", "")},
			require: true,
			wantErr: "goal settings version is empty",
		},
		"best effort preserves local goal after nil response": {},
		"required rejects nil response": {
			require: true,
			wantErr: "machine is nil",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			goal := testGoal("1.35.1", "")
			goal.NodeLabels = map[string]string{"source": "local"}
			client := &ensureMachineClient{machine: tt.machine, getResultSet: true}
			task := EnsureMachine(client, &goal, tt.require, slog.New(slog.NewTextHandler(io.Discard, nil)))

			err := task.Do(t.Context())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Do() error = %v, want containing %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Do() error = %v", err)
			}
			if goal.SettingsVersion != "" || goal.NodeLabels["source"] != "local" {
				t.Fatalf("local goal changed after invalid response: %#v", goal)
			}
			if client.createCalls != 0 {
				t.Fatalf("Create() calls = %d, want 0", client.createCalls)
			}
		})
	}
}

type ensureMachineClient struct {
	machine      *Machine
	getResultSet bool
	createResult *Machine
	getErr       error
	createErr    error
	createCalls  int
	createdGoal  GoalState
}

func (c *ensureMachineClient) Get(context.Context) (*Machine, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	if c.getResultSet || c.machine != nil {
		return c.machine, nil
	}
	return nil, &NotFoundError{Resource: "machine"}
}

func (c *ensureMachineClient) Create(_ context.Context, goal GoalState) (*Machine, error) {
	c.createCalls++
	c.createdGoal = goal
	if c.createErr != nil {
		return nil, c.createErr
	}
	if c.createResult != nil {
		return c.createResult, nil
	}
	return &Machine{Goal: MachineGoal{
		KubernetesVersion: goal.KubernetesVersion,
		SettingsVersion:   goal.SettingsVersion,
		MaxPods:           ptr(goal.MaxPods),
		NodeLabels:        goal.NodeLabels,
		NodeTaints:        goal.NodeTaints,
		KubeletConfig: MachineKubeletConfig{
			ImageGCHighThreshold: ptr(goal.KubeletConfig.ImageGCHighThreshold),
			ImageGCLowThreshold:  ptr(goal.KubeletConfig.ImageGCLowThreshold),
		},
	}}, nil
}

func (c *ensureMachineClient) PatchStatus(context.Context, Status) error {
	return nil
}

var _ MachineClient = (*ensureMachineClient)(nil)
