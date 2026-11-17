package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

var _ NodeProvider = noneProvider{}
var _ nodeOperator = (*noneNodeOperator)(nil)

type noneProvider struct{}

func (noneProvider) Mode() isolation.Mode                                                 { return isolation.ModeNone }
func (noneProvider) RuntimeChecks(*config.Config, *slog.Logger, bool) []preflight.Checker { return nil }
func (noneProvider) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service"}
}
func (noneProvider) SupportsDisconnectTolerance() bool             { return false }
func (noneProvider) NSpawnRuntime() nspawnRuntime                  { return nil }
func (noneProvider) ValidateAgentActivation(context.Context) error { return nil }
func (noneProvider) ValidateActiveSlot(string) error               { return nil }
func (noneProvider) SynchronizeAgent(string, string) error         { return nil }
func (noneProvider) ValidateAgent(string, string) error            { return nil }
func (noneProvider) RollbackAgent(string, string) error            { return nil }
func (noneProvider) CheckAgentCandidate(ctx context.Context, candidate string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, candidate, "supports-isolation", string(isolation.ModeNone)).Run(); err != nil { //nolint:gosec // candidate is verified before this capability probe
		return fmt.Errorf("candidate agent cannot operate isolation mode none: %w", err)
	}
	return nil
}
func (noneProvider) ValidateResetState(state *State) error {
	if state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return fmt.Errorf("none node has no valid applied configuration for reset")
	}
	return nil
}

func (noneProvider) ResetNode(log *slog.Logger) phases.Task {
	return resetNoneNode(log, &noneops.Teardown{Log: log})
}

func (noneProvider) RewriteNodeUnits(ctx context.Context, log *slog.Logger, _ *config.Config, state stateStore) error {
	applied, err := state.Load(ctx)
	if err != nil {
		return err
	}
	if applied == nil || applied.NodeConfig == nil {
		return fmt.Errorf("none node has no applied configuration")
	}
	gs, err := newNoneRuntime(noneops.Params{Log: log, Config: applied.NodeConfig}).GoalState()
	if err != nil {
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
	return syncDirs(systemdSystemDir, filepath.Dir(recoveryScriptPathUnder(hostroot.Resolve())))
}

func (noneProvider) ValidateEnvironment(containerized bool) error {
	if containerized {
		return fmt.Errorf("none isolation requires the host-installed agent; the containerized agent requires host containerd")
	}
	return nil
}

func (noneProvider) BootstrapPresent(log *slog.Logger) bool {
	return (&noneops.Teardown{Log: log}).Present()
}

func (p noneProvider) CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error {
	if applied != nil || p.BootstrapPresent(log) {
		return fmt.Errorf("host contains an installed or partially reset none node; reset it before bootstrapping")
	}
	if err := host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNone); err != nil {
		return err
	}
	backend := none.NewDefault(log)
	backend.LookPath = exec.LookPath
	return backend.Preflight()
}

func (noneProvider) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, slot)
	if err != nil {
		return nil, nil, nil, err
	}
	gs, err = newNoneRuntime(noneops.Params{Log: log, Config: ac, GoalState: gs}).GoalState()
	return ac, gs, archives, err
}

func (noneProvider) SetupHost(cfg *config.Config, log *slog.Logger, _ bool) phases.Task {
	return phases.Serial(log,
		host.InstallPackagesFor(log, isolation.ModeNone),
		noneops.LoadKernelModules(log, nil),
		phases.Parallel(log,
			host.ConfigureOS(log), host.ConfigureNFTablesFor(log, isolation.ModeNone),
			host.DisableDocker(log), host.DisableSwap(log), host.RemoveAPTHardening(log),
			hostrouting.Configure(cfg, log),
		),
	)
}

func (noneProvider) StartNode(cfg *config.Config, log *slog.Logger, _ string, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging, store stateStore, state *State) phases.Task {
	return hostnode.Func("start-none-node", func(ctx context.Context) error {
		ac := config.ToAgentConfig(cfg, noneops.Slot)
		registration := *state
		registration.NodeConfig = ac
		ac = registrationState(&registration).NodeConfig
		gs.NodeStart.Kubelet.NodeLabels = ac.Kubelet.Labels
		node := newNoneRuntime(noneops.Params{Log: log, Config: ac, GoalState: gs})
		if err := stageContainerImageArchiveBindSource(log, archives).Do(ctx); err != nil {
			return err
		}
		if err := node.Provision(ctx); err != nil {
			return err
		}
		containerdChanged, kubeletChanged, err := node.Configure(ctx)
		if err != nil {
			return err
		}
		if err := prepareNoneDependencies(ctx, log, cfg); err != nil {
			return err
		}
		if err := node.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
			return err
		}
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		if err := installNoneNPD(ctx, log, cfg, gs); err != nil {
			return err
		}
		if err := npd.Start(log, cfg, gs.NodeStart.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx); err != nil {
			return err
		}
		seedNoneState(state, ac, noneNPDVersion(cfg))
		return saveState(store, state).Do(ctx)
	})
}

func resetNoneNode(log *slog.Logger, teardown *noneops.Teardown) phases.Task {
	return phases.Serial(log,
		removeNoneHostUnit(log, npd.UnitName),
		resetNoneRuntime(log, teardown, hostnode.ResetCNIState),
		reset.CleanupNetwork(log),
		reset.RemoveWireGuardKeys(log),
		reset.RemoveNFTablesFlush(log),
		removeHostHelpers(log),
		removeNoneHostUnit(log, "static-routes.service"),
		removeNoneHostUnit(log, "check-route-overlap.service"),
		hostnode.Func("remove-host-npd-config", func(context.Context) error {
			return removeIfPresent("/etc/node-problem-detector/kernel-monitor.json")
		}),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
	)
}

func resetNoneRuntime(log *slog.Logger, teardown *noneops.Teardown, resetCNI func(string) error) phases.Task {
	return phases.Serial(log,
		teardown.Stop(),
		teardown.Destroy(),
		hostnode.Func("remove-none-cni-state", func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Destroy ends shims and pod cgroups before their IPAM reservations go.
			if err := resetCNI(teardown.Root); err != nil {
				return fmt.Errorf("reset none CNI state: %w", err)
			}
			return nil
		}),
		teardown.RemoveConfig(),
	)
}

func removeNoneHostUnit(log *slog.Logger, unit string) phases.Task {
	return hostnode.Func("remove-"+unit, func(ctx context.Context) error {
		path := filepath.Join(systemdSystemDir, unit)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		if err := utilexec.RunCmd(ctx, log, utilexec.Systemctl(), "disable", "--now", unit); err != nil {
			return err
		}
		return os.Remove(path)
	})
}

func noneNPDVersion(cfg *config.Config) string {
	if strings.TrimSpace(cfg.Bootstrap.OfflineArtifacts.Source) != "" {
		return ""
	}
	if cfg.Npd.Version != "" {
		return cfg.Npd.Version
	}
	return npd.DefaultVersion
}

func noneRequiredServices(cfg *config.Config) []string {
	services := append([]string{"check-route-overlap.service"}, cfg.Bootstrap.AdditionalRequiredServices...)
	if len(cfg.HostRouting.StaticRoutes.Routes) > 0 {
		services = append(services, "static-routes.service")
	}
	return services
}

func configureNoneDependencies(log *slog.Logger, services []string) phases.Task {
	return hostnode.Func("configure-none-dependencies", func(ctx context.Context) error {
		units := strings.Join(services, " ")
		content := []byte("[Unit]\nRequires=" + units + "\nAfter=" + units + "\n")
		for _, unit := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS} {
			path := filepath.Join(systemdSystemDir, unit+".d", "10-flex-dependencies.conf")
			if err := utilio.WriteFile(path, content, 0o644); err != nil {
				return err
			}
		}
		return utilexec.ReloadSystemd(ctx, log)
	})
}
