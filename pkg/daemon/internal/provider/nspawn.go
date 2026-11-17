package provider

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

var _ NodeProvider = NSpawn{}

// NSpawn implements the systemd-nspawn worker lifecycle.
type NSpawn struct {
	Services   NSpawnServices
	BinaryPath func(slot string) string
}

// NSpawnAgentBinaryPath locates the credential helper in an nspawn worker.
func NSpawnAgentBinaryPath(machine string) string {
	return filepath.Join("/var/lib/machines", machine, "usr", "local", "bin", "aks-flex-node")
}

func (NSpawn) Mode() isolation.Mode                                                       { return isolation.ModeNSpawn }
func (NSpawn) ValidateEnvironment(containerized bool) error                               { return nil }
func (NSpawn) ValidateResetState(state *State) error                                      { return nil }
func (NSpawn) BootstrapPresent(log *slog.Logger) bool                                     { return false }
func (NSpawn) CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error { return nil }
func (NSpawn) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	return config.ResolveMachineGoalState(ctx, log, cfg, slot)
}
func (p NSpawn) RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker {
	return []preflight.Checker{p.Services.CheckMachinesTarget(log, containerized || strings.TrimSpace(cfg.Bootstrap.OfflineArtifacts.Source) != "")}
}
func (NSpawn) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service", "machines.target"}
}
func (NSpawn) SupportsDisconnectTolerance() bool { return true }
func (p NSpawn) NSpawnRuntime() NSpawnRuntime    { return p }
func (p NSpawn) ConvergeContainerAgent(ctx context.Context, log *slog.Logger, state StateLoader) (func(ctx context.Context) error, error) {
	return p.Services.ConvergeContainerAgent(ctx, log, state)
}
func (NSpawn) ValidateAgentActivation(ctx context.Context) error { return nil }
func (NSpawn) ValidateActiveSlot(slot string) error {
	if slot != goalstates.NSpawnMachineKube1 && slot != goalstates.NSpawnMachineKube2 {
		return fmt.Errorf("no valid active node slot %q", slot)
	}
	return nil
}
func (NSpawn) CheckAgentCandidate(ctx context.Context, candidate string) error { return nil }
func (p NSpawn) SynchronizeAgent(source string, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	return p.Services.CopyExecutable(source, p.BinaryPath(slot))
}
func (p NSpawn) ValidateAgent(source string, slot string) error {
	if err := p.ValidateActiveSlot(slot); err != nil {
		return err
	}
	equal, err := p.Services.FilesHaveEqualSHA256(source, p.BinaryPath(slot))
	if err != nil {
		return fmt.Errorf("verify synchronized nspawn agent binary: %w", err)
	}
	if !equal {
		return fmt.Errorf("upgraded host and nspawn agent binaries do not match")
	}
	return nil
}
func (p NSpawn) RollbackAgent(source string, slot string) error {
	if slot != goalstates.NSpawnMachineKube1 && slot != goalstates.NSpawnMachineKube2 {
		return nil
	}
	return p.SynchronizeAgent(source, slot)
}
func (NSpawn) SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	var tasks []phases.Task
	// A container's package manager installs into its image, not the host.
	if !containerized {
		tasks = append(tasks, host.InstallPackages(log))
	}
	tasks = append(tasks, phases.Parallel(log, host.ConfigureOS(log), host.ConfigureNFTables(log), host.DisableDocker(log), host.DisableSwap(log), host.HardenAPT(log), hostrouting.Configure(cfg, log)))
	return phases.Serial(log, tasks...)
}
func (p NSpawn) StartNode(params StartParams) phases.Task {
	cfg, log, gs := params.Config, params.Log, params.GoalState
	return phases.Serial(log,
		p.Services.EnsureLifecycleUnits(log),
		StageContainerImageArchiveBindSource(log, params.ContainerImageArchives),
		rootfs.Provision(log, gs.RootFS),
		p.Services.ConfigureDependencies(log, gs.RootFS, cfg.Bootstrap.AdditionalRequiredServices),
		phases.Parallel(log, npd.Download(log, cfg, gs.RootFS.Root()), p.Services.InstallBinary(gs.RootFS.Root())),
		nodestart.StartNode(log, gs.NodeStart), nodestart.WaitForKubelet(log, params.MachineName),
		npd.Start(log, cfg, gs.RootFS.Root(), gs.NodeStart), p.Services.SaveState(params.Store, params.State),
	)
}
func (p NSpawn) ResetNode(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		phases.Parallel(log, nodestop.StopNode(log, goalstates.NSpawnMachineKube1), nodestop.StopNode(log, goalstates.NSpawnMachineKube2)),
		phases.Parallel(log, reset.CleanupMachine(log, goalstates.NSpawnMachineKube1), reset.CleanupMachine(log, goalstates.NSpawnMachineKube2)),
		phases.Parallel(log, reset.CleanupNetwork(log), reset.RemoveWireGuardKeys(log), p.Services.CleanupLegacyBridgeCNI(log)),
		p.Services.RemoveHostHelpers(log), reset.ReloadSystemd(log), config.RemoveRuntimeDirs(log),
	)
}
func (p NSpawn) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, state StateStore) error {
	active, err := p.Services.ActiveMachineFromStore(ctx, state)
	if err != nil {
		return err
	}
	machineCfg := cfg.DeepCopy()
	if active.State.AppliedKubernetesVersion != "" {
		machineCfg.Components.Kubernetes = active.State.AppliedKubernetesVersion
	}
	agentCfg := config.ToAgentConfig(machineCfg, active.Name)
	rootFS, err := goalstates.ResolveNSpawnConfig(agentCfg, active.Name)
	if err != nil {
		return fmt.Errorf("resolve machine nspawn config: %w", err)
	}
	if err := phases.Serial(log, NSpawnLifecycleTasks(log, rootFS)...).Do(ctx); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := goalstates.ResolveMachine(log, agentCfg, active.Name, nil)
		if err != nil {
			return fmt.Errorf("resolve machine goal state: %w", err)
		}
		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("reload systemd after moving to the host root: %w", err)
	}
	return p.Services.SyncDirs(goalstates.SystemdSystemDir, filepath.Dir(rootFS.ServiceOverrideFile), filepath.Dir(p.Services.RecoveryScriptPathUnder(hostroot.Resolve())), filepath.Dir(goalstates.ResolveHostPaths().NSpawnLifecycleBinary))
}
func (p NSpawn) NewOperator(ctx context.Context, cfg *config.Config, store StateStore) (Operator, error) {
	return p.Services.NewOperator(ctx, cfg, store, p)
}

// NSpawnLifecycleTasks installs the current helper before regenerating legacy hooks.
func NSpawnLifecycleTasks(log *slog.Logger, rootFS *goalstates.RootFS) []phases.Task {
	return []phases.Task{rootfs.EnsureNSpawnLifecycleHelperFor(rootFS), rootfs.EnsureNSpawnConfig(log, rootFS)}
}
