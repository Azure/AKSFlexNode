package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ContainerRuntime exposes the SDK state needed for artifact installation and persistence.
type ContainerRuntime interface {
	GoalState() (*goalstates.MachineGoalState, error)
	Backend() *container.Backend
}

func (Container) SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	return phases.Serial(log,
		host.InstallPackagesFor(log, isolation.ModeContainer),
		noneops.LoadKernelModules(log, nil),
		phases.Parallel(log,
			host.ConfigureOS(log), host.ConfigureNFTablesFor(log, isolation.ModeContainer),
			phases.Serial(log, host.DisableDocker(log), host.ConfigureDocker(log)),
			host.DisableContainerd(log), host.DisableKubelet(log),
			host.DisableSwap(log), host.RemoveAPTHardening(log),
			hostrouting.Configure(cfg, log),
		),
	)
}

// NewContainerNode retains Flex's stable lifecycle helper instead of installing
// the SDK agent over it.
func NewContainerNode(p containerops.Params) *containerops.Node {
	p.HelperPath = config.HostBinaryPath()
	p.ConfigSnapshotDir = filepath.Join(config.ConfigDir, "container-upgrade-config")
	p.InstallHelper = func(ctx context.Context) error {
		info, err := os.Stat(p.HelperPath)
		if err != nil {
			return fmt.Errorf("inspect Flex node lifecycle executable: %w", err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("flex node lifecycle helper %s is not executable", p.HelperPath)
		}
		return nil
	}
	return containerops.New(p)
}

// ContainerArtifacts installs the Flex helper and NPD before kubelet can start.
func ContainerArtifacts(cfg *config.Config, log *slog.Logger, installBinary func(root isolation.NodeRoot) phases.Task) func(ctx context.Context, goal *goalstates.MachineGoalState) error {
	return func(ctx context.Context, gs *goalstates.MachineGoalState) error {
		return phases.Serial(log,
			installBinary(gs.RootFS.Root()),
			npd.Download(log, cfg, gs.RootFS.Root()),
		).Do(ctx)
	}
}

// WrapContainerArtifactUnpack stages Flex artifacts through the SDK's unpack hook
// because Upgrade calls Prepare internally.
func WrapContainerArtifactUnpack(node ContainerRuntime, install func(ctx context.Context, goal *goalstates.MachineGoalState) error) error {
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	backend := node.Backend()
	unpack := backend.Unpack
	backend.Unpack = func(ctx context.Context, spec isolation.NodeSpec, dir string) error {
		if err := unpack(ctx, spec, dir); err != nil {
			return err
		}
		if err := install(ctx, gs); err != nil {
			return fmt.Errorf("install Flex container artifacts: %w", err)
		}
		return nil
	}
	return nil
}

func (p Container) StartNode(params StartParams) phases.Task {
	cfg, log, slot, gs := params.Config, params.Log, params.MachineName, params.GoalState
	store, state := params.Store, params.State
	return hostnode.Func("start-container-node", func(ctx context.Context) error {
		ac := config.ToAgentConfig(cfg, slot).DeepCopy()
		node := NewContainerNode(containerops.Params{Log: log, Config: ac, GoalState: gs, Slot: slot})
		if err := WrapContainerArtifactUnpack(node, ContainerArtifacts(cfg, log, p.Host.InstallBinary)); err != nil {
			return err
		}
		resolved, err := node.GoalState()
		if err != nil {
			return err
		}
		node.Backend().UnitRequires = append(node.Backend().UnitRequires, NoneRequiredServices(cfg)...)
		if err := StageContainerImageArchiveBindSource(log, params.ContainerImageArchives).Do(ctx); err != nil {
			return err
		}
		running, err := node.Running(ctx)
		if err != nil {
			return err
		}
		if !running {
			if _, err := node.Prepare(ctx); err != nil {
				return err
			}
		}
		containerdChanged, kubeletChanged, err := node.Configure(ctx)
		if err != nil {
			return err
		}
		if err := node.Start(ctx, running, containerdChanged, kubeletChanged); err != nil {
			return err
		}
		if err := npd.Start(log, cfg, resolved.NodeStart.Root(), resolved.NodeStart).Do(ctx); err != nil {
			return err
		}
		if err := SyncContainerFilesystems(ctx, node); err != nil {
			return fmt.Errorf("persist bootstrapped container: %w", err)
		}
		state.Isolation, state.ActiveMachine, state.NodeConfig = isolation.ModeContainer, slot, ac
		return p.Host.SaveState(store, state).Do(ctx)
	})
}

func (p Container) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, store StateStore) error {
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	startup, err := p.Startup.Load()
	if err != nil {
		return err
	}
	if startup != nil {
		if startup.Phase == "preparing" {
			return fmt.Errorf("container preparation must recover before moving the host root")
		}
		state = startup.Next
	}
	if !ValidContainerState(state) {
		return fmt.Errorf("container node has no valid applied configuration")
	}
	node := NewContainerNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	node.Backend().UnitRequires = append(node.Backend().UnitRequires, NoneRequiredServices(cfg)...)
	if err := node.RewriteUnit(ctx); err != nil {
		return err
	}
	if gs.NodeStart.LocalDNS.Enabled {
		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return err
		}
	}
	if err := utilexec.ReloadSystemd(ctx, log); err != nil {
		return err
	}
	return p.Host.SyncDirs(goalstates.SystemdSystemDir, filepath.Dir(p.Host.RecoveryScriptPathUnder(p.PlannedHostRoot())))
}

func (p Container) ResetNode(log *slog.Logger) phases.Task {
	owned := *p.Teardown
	owned.Log = log
	teardown := &owned
	// Serial is intentional: shared host data and persisted state survive a
	// failed stop/destroy so reset can be retried without losing ownership.
	return phases.Serial(log,
		teardown.Stop(), teardown.Destroy(), teardown.RemoveConfig(),
		reset.CleanupNetwork(log), reset.RemoveWireGuardKeys(log), reset.RemoveNFTablesFlush(log),
		p.Host.RemoveHostHelpers(log),
		RemoveHostUnit(log, "static-routes.service"),
		RemoveHostUnit(log, "check-route-overlap.service"),
		reset.ReloadSystemd(log), config.RemoveRuntimeDirs(log),
	)
}
