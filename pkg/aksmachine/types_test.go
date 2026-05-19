package aksmachine

import (
	"context"
	"testing"
)

type fakeMachineClient struct{}

func (fakeMachineClient) Create(context.Context, GoalState) (*Machine, error) { return nil, nil }
func (fakeMachineClient) Get(context.Context) (*Machine, error)               { return nil, nil }
func (fakeMachineClient) PatchStatus(context.Context, Status) error           { return nil }

func TestFakeMachineClientImplementsInterface(t *testing.T) {
	t.Parallel()

	var _ MachineClient = fakeMachineClient{}
}
