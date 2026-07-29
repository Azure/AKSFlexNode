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
			goal := GoalState{KubernetesVersion: "1.35.1"}
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
			goal := GoalState{KubernetesVersion: "1.35.1"}
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

	goal := GoalState{KubernetesVersion: "1.35.1", SettingsVersion: "1.35.1"}
	client := &ensureMachineClient{createResult: &Machine{Goal: GoalState{
		KubernetesVersion: "1.35.1",
		SettingsVersion:   "etag-created",
	}}}
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
}

func TestEnsureMachineAdoptsExistingGoal(t *testing.T) {
	t.Parallel()

	goal := GoalState{
		KubernetesVersion: "1.35.1",
		SettingsVersion:   "1.35.1",
		MaxPods:           30,
		NodeLabels:        map[string]string{"source": "local"},
		NodeTaints:        []string{"local=true:NoSchedule"},
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
	client := &ensureMachineClient{machine: &Machine{Goal: GoalState{
		KubernetesVersion: "1.35.1",
		SettingsVersion:   "etag-42",
		MaxPods:           110,
		NodeLabels:        map[string]string{"source": "remote"},
		NodeTaints:        []string{"remote=true:NoSchedule"},
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 70,
			ImageGCLowThreshold:  60,
		},
	}}}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Create() calls = %d, want 0", client.createCalls)
	}
	if goal.SettingsVersion != "etag-42" || goal.KubernetesVersion != "1.35.1" {
		t.Fatalf("version goal = %#v", goal)
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

	goal := GoalState{KubernetesVersion: "1.35.1", SettingsVersion: "1.35.1"}
	client := &ensureMachineClient{
		machine: &Machine{Goal: GoalState{
			KubernetesVersion: "1.34.0",
			SettingsVersion:   "etag-remote",
			NodeLabels:        map[string]string{"source": "remote"},
			NodeTaints:        []string{"remote=true:NoSchedule"},
		}},
	}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if client.createCalls != 0 {
		t.Fatalf("Create() calls = %d, want 0", client.createCalls)
	}
	if goal.KubernetesVersion != "1.34.0" || goal.SettingsVersion != "etag-remote" {
		t.Fatalf("version goal = %#v", goal)
	}
	if goal.NodeLabels["source"] != "remote" || goal.NodeTaints[0] != "remote=true:NoSchedule" {
		t.Fatalf("remote goal was not adopted: %#v", goal)
	}
}

func TestEnsureMachineRejectsMissingSettingsVersion(t *testing.T) {
	t.Parallel()

	goal := GoalState{KubernetesVersion: "1.35.1"}
	client := &ensureMachineClient{
		machine: &Machine{Goal: GoalState{KubernetesVersion: "1.35.1"}},
	}
	task := EnsureMachine(client, &goal, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := task.Do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "goal settings version is empty") {
		t.Fatalf("Do() error = %v, want missing settings version", err)
	}
}

type ensureMachineClient struct {
	machine      *Machine
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
	if c.machine != nil {
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
	return &Machine{Goal: goal}, nil
}

func (c *ensureMachineClient) PatchStatus(context.Context, Status) error {
	return nil
}

var _ MachineClient = (*ensureMachineClient)(nil)
