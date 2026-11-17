package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// ContainerStartup records local completion after an SDK container upgrade.
type ContainerStartup struct {
	Phase    string `json:"phase"`
	Next     *State `json:"next"`
	Previous *State `json:"previous"`
}

// ContainerStartupReader reads pending container completion before agent activation.
type ContainerStartupReader interface {
	Load() (*ContainerStartup, error)
}

var _ NodeProvider = Container{}

// Container implements the OCI worker lifecycle.
type Container struct {
	Services           ContainerServices
	Startup            ContainerStartupReader
	StartupPath        string
	ActivationLockPath string
	BinaryPath         func(slot string) string
	Teardown           *containerops.Teardown
}

// ContainerAgentBinaryPath locates the credential helper in an OCI slot.
func ContainerAgentBinaryPath(slot string) string {
	return filepath.Join(container.Layout{}.RootfsDir(slot), "usr/local/bin/aks-flex-node")
}

func (Container) Mode() isolation.Mode { return isolation.ModeContainer }
func (Container) ValidateEnvironment(containerized bool) error {
	if containerized {
		return fmt.Errorf("container isolation requires the host-installed agent; the containerized agent requires host containerd")
	}
	return nil
}
func (p Container) BootstrapPresent(log *slog.Logger) bool {
	// PREPARING can precede applied state and SDK footprints. An unreadable
	// journal also retains ownership so activation fails closed.
	if _, err := os.Lstat(p.StartupPath); !os.IsNotExist(err) {
		return true
	}
	teardown := *p.Teardown
	teardown.Log = log
	return teardown.Present()
}
func (p Container) CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error {
	if applied != nil || p.BootstrapPresent(log) {
		return fmt.Errorf("host contains an installed or partially reset container node; reset it before bootstrapping")
	}
	return p.Services.CheckBootstrap(ctx, log)
}
func (p Container) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, slot)
	if err != nil {
		return nil, nil, nil, err
	}
	gs, err = p.Services.NewNode(containerops.Params{Log: log, Config: ac, GoalState: gs, Slot: slot}).GoalState()
	return ac, gs, archives, err
}
func (p Container) SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	return p.Services.SetupHost(cfg, log)
}
func (p Container) StartNode(params StartParams) phases.Task { return p.Services.StartNode(params) }
func (Container) RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker {
	return nil
}
func (Container) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service"}
}
func (Container) SupportsDisconnectTolerance() bool { return false }
func (Container) NSpawnRuntime() NSpawnRuntime      { return nil }
func (p Container) ResetNode(log *slog.Logger) phases.Task {
	teardown := *p.Teardown
	teardown.Log = log
	return p.Services.ResetNode(log, &teardown)
}
func (Container) ValidateResetState(state *State) error {
	if !ValidContainerState(state) {
		return fmt.Errorf("container node has no valid applied configuration for reset")
	}
	return nil
}
func (p Container) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, store StateStore) error {
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	return p.Services.RewriteNodeUnits(ctx, log, cfg, state, p.Startup)
}
func (p Container) ValidateAgentActivation(ctx context.Context) error {
	return RequireNoContainerStartupForAgentActivation(p.Startup)
}
func (Container) ValidateActiveSlot(slot string) error {
	if slot != goalstates.NSpawnMachineKube1 && slot != goalstates.NSpawnMachineKube2 {
		return fmt.Errorf("no valid active node slot %q", slot)
	}
	return nil
}
func (Container) CheckAgentCandidate(ctx context.Context, candidate string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, candidate, "supports-isolation", string(isolation.ModeContainer)).Run(); err != nil { //nolint:gosec // candidate is verified before this capability probe
		return fmt.Errorf("candidate agent cannot operate isolation mode container: %w", err)
	}
	return nil
}
func (p Container) SynchronizeAgent(source string, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	return p.Services.CopyExecutable(source, p.BinaryPath(slot))
}
func (p Container) ValidateAgent(source string, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	equal, err := p.Services.FilesHaveEqualSHA256(source, p.BinaryPath(slot))
	if err != nil {
		return fmt.Errorf("verify synchronized container agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and container agent binaries do not match")
	}
	return nil
}
func (p Container) RollbackAgent(source string, slot string) error {
	if slot != goalstates.NSpawnMachineKube1 && slot != goalstates.NSpawnMachineKube2 {
		return nil
	}
	return p.SynchronizeAgent(source, slot)
}
func (p Container) NewOperator(ctx context.Context, cfg *config.Config, store StateStore) (Operator, error) {
	if !cfg.Bootstrap.ExperimentalContainerIsolation {
		return nil, fmt.Errorf("container isolation requires bootstrap.experimentalContainerIsolation=true")
	}
	return p.Services.NewOperator(ctx, cfg, store, p)
}

// ValidContainerState checks persisted container slot and configuration ownership.
func ValidContainerState(state *State) bool {
	return state != nil && state.IsolationMode() == isolation.ModeContainer &&
		(state.ActiveMachine == goalstates.NSpawnMachineKube1 || state.ActiveMachine == goalstates.NSpawnMachineKube2) &&
		state.NodeConfig != nil && state.NodeConfig.Isolation == string(isolation.ModeContainer) && state.NodeConfig.MachineName == state.ActiveMachine &&
		state.NodeConfig.Experimental != nil && state.NodeConfig.Experimental.ContainerIsolation
}

// RequireNoContainerStartupForAgentActivation fences the stopped source while
// local completion may still be pending for its running replacement.
func RequireNoContainerStartupForAgentActivation(startup ContainerStartupReader) error {
	pending, err := startup.Load()
	if err != nil {
		return fmt.Errorf("inspect container startup before agent activation: %w", err)
	}
	if pending != nil {
		return fmt.Errorf("agent activation requires container startup to complete (phase %s)", pending.Phase)
	}
	return nil
}
