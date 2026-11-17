package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

var _ NodeProvider = nspawnProvider{}
var _ nodeOperator = (*nspawnNodeOperator)(nil)

type nspawnProvider struct {
	binaryPath func(string) string
}

func activeNspawnAgentBinaryPath(machine string) string {
	return filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
}

func (nspawnProvider) Mode() isolation.Mode                                       { return isolation.ModeNSpawn }
func (nspawnProvider) ValidateEnvironment(bool) error                             { return nil }
func (nspawnProvider) ValidateResetState(*State) error                            { return nil }
func (nspawnProvider) BootstrapPresent(*slog.Logger) bool                         { return false }
func (nspawnProvider) CheckBootstrap(context.Context, *slog.Logger, *State) error { return nil }
func (nspawnProvider) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	return config.ResolveMachineGoalState(ctx, log, cfg, slot)
}
func (nspawnProvider) RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker {
	return []preflight.Checker{CheckMachinesTarget(log, containerized || strings.TrimSpace(cfg.Bootstrap.OfflineArtifacts.Source) != "")}
}
func (nspawnProvider) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service", "machines.target"}
}
func (nspawnProvider) SupportsDisconnectTolerance() bool { return true }
func (p nspawnProvider) NSpawnRuntime() nspawnRuntime    { return p }
func (nspawnProvider) ConvergeContainerAgent(ctx context.Context, log *slog.Logger, state agentUpgradeStateLoader) (func(context.Context) error, error) {
	convergence := newContainerAgentConvergence(log, state)
	held, err := convergence.converge(ctx)
	if err != nil {
		return nil, err
	}
	if held {
		return convergence.writeUnits, nil
	}
	return noHeldAssets, nil
}
func (nspawnProvider) ValidateAgentActivation(context.Context) error { return nil }
func (nspawnProvider) ValidateActiveSlot(slot string) error {
	if !validNspawnMachine(slot) {
		return fmt.Errorf("no valid active node slot %q", slot)
	}
	return nil
}
func (nspawnProvider) CheckAgentCandidate(context.Context, string) error { return nil }
func (p nspawnProvider) SynchronizeAgent(source, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	return copyExecutable(source, p.binaryPath(slot))
}
func (p nspawnProvider) ValidateAgent(source, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	equal, err := filesHaveEqualSHA256(source, p.binaryPath(slot))
	if err != nil {
		return fmt.Errorf("verify synchronized nspawn agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and nspawn agent binaries do not match")
	}
	return nil
}
func (p nspawnProvider) RollbackAgent(source, slot string) error {
	if !validNspawnMachine(slot) {
		return nil
	}
	return p.SynchronizeAgent(source, slot)
}

func (nspawnProvider) SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	var tasks []phases.Task
	// In a container, the package manager on PATH is the image's, so it would install into the
	// container rather than the host.
	if !containerized {
		tasks = append(tasks, host.InstallPackages(log))
	}
	tasks = append(tasks, phases.Parallel(log,
		host.ConfigureOS(log),
		host.ConfigureNFTables(log),
		host.DisableDocker(log),
		host.DisableSwap(log),
		host.HardenAPT(log),
		hostrouting.Configure(cfg, log),
	))
	return phases.Serial(log, tasks...)
}

func (nspawnProvider) StartNode(
	cfg *config.Config,
	log *slog.Logger,
	machineName string,
	gs *goalstates.MachineGoalState,
	containerImageArchives *goalstates.ContainerImageArchiveStaging,
	store stateStore,
	state *State,
) phases.Task {
	return phases.Serial(log,
		// Before anything starts the machine: its hooks start these units.
		EnsureNSpawnLifecycleUnits(log),
		stageContainerImageArchiveBindSource(log, containerImageArchives),
		rootfs.Provision(log, gs.RootFS),
		ConfigureNSpawnDependencies(log, gs.RootFS, cfg.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log,
			npd.Download(log, cfg, gs.RootFS.Root()),
			InstallBinary(gs.RootFS.Root()),
		),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, machineName),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart, isolation.ModeNSpawn),
		saveState(store, state),
	)
}
