package daemon

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

var _ NodeProvider = containerProvider{}
var _ nodeOperator = (*containerNodeOperator)(nil)

type containerProvider struct {
	startup            containerStartupReader
	startupPath        string
	activationLockPath string
	binaryPath         func(string) string
	teardown           *containerops.Teardown
}

func newContainerProvider() NodeProvider {
	startup := defaultContainerStartupStore()
	return containerProvider{
		startup: startup, startupPath: startup.path, activationLockPath: agentUpgradeLockPath,
		binaryPath: activeContainerAgentBinaryPath, teardown: &containerops.Teardown{},
	}
}

func activeContainerAgentBinaryPath(slot string) string {
	return filepath.Join(container.Layout{}.RootfsDir(slot), "usr/local/bin/aks-flex-node")
}

func (containerProvider) Mode() isolation.Mode { return isolation.ModeContainer }
func (containerProvider) ValidateEnvironment(containerized bool) error {
	if containerized {
		return fmt.Errorf("container isolation requires the host-installed agent; the containerized agent requires host containerd")
	}
	return nil
}
func (p containerProvider) BootstrapPresent(log *slog.Logger) bool {
	// PREPARING can precede both Flex applied state and SDK footprints. Any
	// unreadable journal also retains ownership so activation fails closed.
	if _, err := os.Lstat(p.startupPath); !os.IsNotExist(err) {
		return true
	}
	teardown := *p.teardown
	teardown.Log = log
	return teardown.Present()
}
func (p containerProvider) CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error {
	if applied != nil || p.BootstrapPresent(log) {
		return fmt.Errorf("host contains an installed or partially reset container node; reset it before bootstrapping")
	}
	return checkContainerBootstrap(ctx, log)
}
func (containerProvider) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, slot)
	if err != nil {
		return nil, nil, nil, err
	}
	gs, err = newContainerNode(containerops.Params{Log: log, Config: ac, GoalState: gs, Slot: slot}).GoalState()
	return ac, gs, archives, err
}
func (containerProvider) SetupHost(cfg *config.Config, log *slog.Logger, _ bool) phases.Task {
	return setupContainerHost(cfg, log)
}
func (containerProvider) StartNode(params StartParams) phases.Task {
	return startContainerNode(params)
}
func (containerProvider) RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker {
	return nil
}
func (containerProvider) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service"}
}
func (containerProvider) SupportsDisconnectTolerance() bool { return false }
func (containerProvider) NSpawnRuntime() nspawnRuntime      { return nil }
func (p containerProvider) ResetNode(log *slog.Logger) phases.Task {
	teardown := *p.teardown
	teardown.Log = log
	return resetContainerNode(log, &teardown)
}
func (containerProvider) ValidateResetState(state *State) error {
	if !validContainerState(state) {
		return fmt.Errorf("container node has no valid applied configuration for reset")
	}
	return nil
}
func (p containerProvider) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, store stateStore) error {
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	return rewriteContainerHostRootUnits(ctx, log, cfg, state, p.startup)
}
func (p containerProvider) ValidateAgentActivation(ctx context.Context) error {
	return requireNoContainerStartupForAgentActivation(p.startup)
}
func (containerProvider) ValidateActiveSlot(slot string) error {
	if !validActiveMachine(slot) {
		return fmt.Errorf("no valid active node slot %q", slot)
	}
	return nil
}
func (containerProvider) CheckAgentCandidate(ctx context.Context, candidate string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, candidate, "supports-isolation", string(isolation.ModeContainer)).Run(); err != nil { //nolint:gosec // candidate is verified before this capability probe
		return fmt.Errorf("candidate agent cannot operate isolation mode container: %w", err)
	}
	return nil
}
func (p containerProvider) SynchronizeAgent(source, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	return copyExecutable(source, p.binaryPath(slot))
}
func (p containerProvider) ValidateAgent(source, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	equal, err := filesHaveEqualSHA256(source, p.binaryPath(slot))
	if err != nil {
		return fmt.Errorf("verify synchronized container agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and container agent binaries do not match")
	}
	return nil
}
func (p containerProvider) RollbackAgent(source, slot string) error {
	if !validActiveMachine(slot) {
		return nil
	}
	return p.SynchronizeAgent(source, slot)
}
func (p containerProvider) NewOperator(ctx context.Context, cfg *config.Config, store stateStore) (nodeOperator, error) {
	if !cfg.Bootstrap.ExperimentalContainerIsolation {
		return nil, fmt.Errorf("container isolation requires bootstrap.experimentalContainerIsolation=true")
	}
	base, state, err := newOperatorBase(ctx, cfg, store, p)
	if err != nil {
		return nil, err
	}
	return newContainerNodeOperator(base, state, containerStartupStore{path: p.startupPath}, p.activationLockPath)
}
