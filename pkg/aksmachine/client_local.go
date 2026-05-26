//go:build local_e2e

package aksmachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const fileMode = 0o600

const localResourceID = "local-test-machine"

// e2eMachineFilePath is the well-known local machine file used by e2e daemon
// mode and by e2ehelper when simulating AKS RP machine changes.
const e2eMachineFilePath = "/run/aks-flex-node/e2e-machine.json"

// LocalClient implements MachineClient with a JSON file. It is compiled only
// with the local_e2e build tag so GitHub E2E tests can simulate the AKS RP
// machine API by mutating local disk state instead of calling ARM.
//
// The file stores the local Machine JSON shape and always uses
// "local-test-machine" as the created machine ID. Tests can create or replace
// goal state by writing a Machine with Goal.KubernetesVersion and
// Goal.SettingsVersion, patch status by updating Machine.Status, and simulate
// an ARM 404 by deleting the file. Missing files are returned as NotFoundError
// so reset/delete flows can treat local deletion like a missing remote machine.
//
// This client is intentionally not a production AKS RP implementation; it only
// exists to let E2E tests validate daemon behavior without a live Machine API.
type LocalClient struct {
	path string
}

// newLocalClient creates a file-backed MachineClient rooted at path. The client
// stores the full Machine payload in JSON and treats a missing file as a missing
// AKS machine resource.
func newLocalClient(path string) (*LocalClient, error) {
	if path == "" {
		return nil, fmt.Errorf("machine file path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create local machine file directory: %w", err)
	}
	return &LocalClient{path: path}, nil
}

func (c *LocalClient) Create(_ context.Context, desired GoalState) (*Machine, error) {
	machine := &Machine{ID: localResourceID, Goal: desired}
	if err := c.write(machine); err != nil {
		return nil, err
	}
	return machine, nil
}

func (c *LocalClient) Get(context.Context) (*Machine, error) {
	return c.read()
}

func (c *LocalClient) PatchStatus(_ context.Context, status Status) error {
	machine, err := c.read()
	if err != nil {
		return err
	}
	machine.Status = status
	return c.write(machine)
}

func (c *LocalClient) read() (*Machine, error) {
	data, err := os.ReadFile(filepath.Clean(c.path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, &NotFoundError{Resource: c.path}
	}
	if err != nil {
		return nil, fmt.Errorf("read machine file %s: %w", c.path, err)
	}

	var machine Machine
	if err := json.Unmarshal(data, &machine); err != nil {
		return nil, fmt.Errorf("decode machine file %s: %w", c.path, err)
	}
	return &machine, nil
}

func (c *LocalClient) write(machine *Machine) error {
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

var _ MachineClient = (*LocalClient)(nil)

// NewMachineClient creates a MachineClient instance from config.
func NewMachineClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	if cfg.Agent.E2EMode {
		logger.Info("using local file-backed machine client for e2e testing", "path", e2eMachineFilePath)
		return newLocalClient(e2eMachineFilePath)
	}

	return newMachineClientFromConfig(cfg, logger)
}
