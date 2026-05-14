package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/spec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const fileMode = 0o600

const localResourceID = "local-test-machine"

// E2EMachineFilePath is the well-known local machine file used by e2e daemon
// mode and by e2ehelper when simulating AKS RP machine changes.
const E2EMachineFilePath = spec.RuntimeDir + "/e2e-machine.json"

// Client implements aksmachine.MachineClient with a JSON file. It is intended
// for e2e tests that simulate AKS RP by mutating local disk state.
type Client struct {
	path string
}

func NewClient(path string) (*Client, error) {
	if path == "" {
		return nil, fmt.Errorf("machine file path is empty")
	}
	return &Client{path: path}, nil
}

func (c *Client) Create(_ context.Context, desired aksmachine.GoalState) (*aksmachine.Machine, error) {
	machine := &aksmachine.Machine{ID: localResourceID, Goal: desired}
	if err := c.write(machine); err != nil {
		return nil, err
	}
	return machine, nil
}

func (c *Client) Get(context.Context) (*aksmachine.Machine, error) {
	return c.read()
}

func (c *Client) PatchStatus(_ context.Context, status aksmachine.Status) error {
	machine, err := c.read()
	if err != nil {
		return err
	}
	machine.Status = status
	return c.write(machine)
}

func (c *Client) read() (*aksmachine.Machine, error) {
	data, err := os.ReadFile(filepath.Clean(c.path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, &aksmachine.NotFoundError{Resource: c.path}
	}
	if err != nil {
		return nil, fmt.Errorf("read machine file %s: %w", c.path, err)
	}

	var machine aksmachine.Machine
	if err := json.Unmarshal(data, &machine); err != nil {
		return nil, fmt.Errorf("decode machine file %s: %w", c.path, err)
	}
	return &machine, nil
}

func (c *Client) write(machine *aksmachine.Machine) error {
	if machine == nil {
		return fmt.Errorf("machine is nil")
	}
	data, err := json.MarshalIndent(machine, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal machine: %w", err)
	}
	data = append(data, '\n')
	if err := utilio.WriteFile(c.path, data, fileMode); err != nil {
		return fmt.Errorf("write machine file %s: %w", c.path, err)
	}
	return nil
}
