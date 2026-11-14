package daemon

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

func checkContainerBootstrap(ctx context.Context, log *slog.Logger) error {
	if err := host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeContainer); err != nil {
		return err
	}
	// Prepare installs the runtime; all other backend requirements, including
	// systemd 254+, must already hold before bootstrap mutates the host.
	return (&container.Backend{Log: log, RuntimeProvisioned: true}).Preflight()
}

func setupContainerHost(cfg *config.Config, log *slog.Logger) phases.Task {
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

// Flex owns node-lifecycle, so Unbounded must not install its own helper over
// the stable executable used by the container's supervising unit.
func newContainerNode(p containerops.Params) *containerops.Node {
	p.HelperPath = defaultAgentUpgradePaths().BinaryPath
	p.ConfigSnapshotDir = filepath.Join(config.ConfigDir, "container-upgrade-config")
	p.InstallHelper = func(context.Context) error {
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

func containerArtifacts(cfg *config.Config, log *slog.Logger) func(context.Context, *goalstates.MachineGoalState) error {
	return func(ctx context.Context, gs *goalstates.MachineGoalState) error {
		return phases.Serial(log,
			InstallBinaryToRoot(gs.RootFS.Root()),
			npd.DownloadToRoot(log, cfg, gs.RootFS.Root()),
		).Do(ctx)
	}
}

func startContainerNode(cfg *config.Config, log *slog.Logger, slot string, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging, store stateStore, state *State) phases.Task {
	return hostnode.Func("start-container-node", func(ctx context.Context) error {
		registration := *state
		registration.Isolation, registration.ActiveMachine = isolation.ModeContainer, slot
		registration.NodeConfig = config.ToAgentConfig(cfg, slot)
		ac := noneRegistrationState(&registration).NodeConfig
		gs.NodeStart.Kubelet.NodeLabels = ac.Kubelet.Labels
		node := newContainerNode(containerops.Params{Log: log, Config: ac, GoalState: gs, Slot: slot, Install: containerArtifacts(cfg, log)})
		resolved, err := node.GoalState()
		if err != nil {
			return err
		}
		node.Backend().UnitRequires = append(node.Backend().UnitRequires, noneRequiredServices(cfg)...)
		if err := stageContainerImageArchiveBindSource(log, archives).Do(ctx); err != nil {
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
		if err := npd.Start(log, cfg, resolved.NodeStart).Do(ctx); err != nil {
			return err
		}
		state.Isolation, state.ActiveMachine, state.NodeConfig = isolation.ModeContainer, slot, ac
		state.NPDVersion = noneNPDVersion(cfg)
		return saveState(store, state).Do(ctx)
	})
}

func rewriteContainerHostRootUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, state *State) error {
	startup, err := defaultContainerStartupStore().Load()
	if err != nil {
		return err
	}
	if startup != nil {
		if startup.Phase == "preparing" {
			return fmt.Errorf("container preparation must recover before moving the host root")
		}
		state = startup.Next
	}
	if !validContainerState(state) {
		return fmt.Errorf("container node has no valid applied configuration")
	}
	node := newContainerNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	node.Backend().UnitRequires = append(node.Backend().UnitRequires, noneRequiredServices(cfg)...)
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
	return syncDirs(systemdSystemDir, filepath.Dir(recoveryScriptPathUnder(PlannedHostRoot())))
}

func resetContainerNode(log *slog.Logger, teardown *containerops.Teardown) phases.Task {
	// Serial is intentional: shared host data and persisted state survive a
	// failed stop/destroy so reset can be retried without losing ownership.
	return phases.Serial(log,
		teardown.Stop(), teardown.Destroy(), teardown.RemoveConfig(),
		reset.CleanupNetwork(log), reset.RemoveWireGuardKeys(log), reset.RemoveNFTablesFlush(log),
		removeHostHelpers(log),
		removeNoneHostUnit(log, "static-routes.service"),
		removeNoneHostUnit(log, "check-route-overlap.service"),
		reset.ReloadSystemd(log), config.RemoveRuntimeDirs(log),
	)
}
