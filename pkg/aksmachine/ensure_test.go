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
			task := EnsureMachine(client, GoalState{KubernetesVersion: "1.35.1"}, tt.require, slog.New(slog.NewTextHandler(io.Discard, nil)))

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
			task := EnsureMachine(client, GoalState{KubernetesVersion: "1.35.1"}, tt.require, slog.New(slog.NewTextHandler(io.Discard, nil)))

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

type ensureMachineClient struct {
	getErr    error
	createErr error
}

func (c *ensureMachineClient) Get(context.Context) (*Machine, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	return nil, &NotFoundError{Resource: "machine"}
}

func (c *ensureMachineClient) Create(context.Context, GoalState) (*Machine, error) {
	if c.createErr != nil {
		return nil, c.createErr
	}
	return &Machine{}, nil
}

func (c *ensureMachineClient) PatchStatus(context.Context, Status) error {
	return nil
}

var _ MachineClient = (*ensureMachineClient)(nil)
