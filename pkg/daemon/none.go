package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/noneops"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// ValidateIsolationEnvironment rejects node modes this execution environment
// cannot operate. The containerized agent depends on a separate host containerd.
func ValidateIsolationEnvironment(cfg *config.Config) error {
	return validateIsolationEnvironment(cfg, Containerized())
}

func validateIsolationEnvironment(cfg *config.Config, containerized bool) error {
	switch cfg.Bootstrap.IsolationMode() {
	case isolation.ModeNSpawn:
		return nil
	case isolation.ModeNone:
		if containerized {
			return fmt.Errorf("none isolation requires the host-installed agent; the containerized agent requires host containerd")
		}
		return nil
	default:
		return fmt.Errorf("unsupported node isolation %q", cfg.Bootstrap.Isolation)
	}
}

// CheckBootstrapIsolation runs before registration or host mutation, even when
// the optional preflight command was not run.
func CheckBootstrapIsolation(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	if err := ValidateIsolationEnvironment(cfg); err != nil {
		return err
	}
	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	applied, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if applied != nil && applied.isolationMode() != cfg.Bootstrap.IsolationMode() {
		return fmt.Errorf("host has an applied %s node; reset it before changing isolation", applied.isolationMode())
	}
	if (&noneops.Teardown{Log: log}).Present() || applied != nil && applied.isolationMode() == isolation.ModeNone {
		return fmt.Errorf("host contains an installed or partially reset none node; reset it before bootstrapping")
	}
	if cfg.Bootstrap.IsolationMode() == isolation.ModeNSpawn {
		return nil
	}
	if err := host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNone); err != nil {
		return err
	}
	backend := none.NewDefault(log)
	backend.LookPath = exec.LookPath
	return backend.Preflight()
}

// ResolveNodeGoalState applies the filesystem and execution view preflight
// will use, without installing artifacts or changing host configuration.
func ResolveNodeGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, slot)
	if err != nil {
		return nil, nil, nil, err
	}
	if cfg.Bootstrap.IsolationMode() == isolation.ModeNone {
		gs, err = noneops.New(noneops.Params{
			Log: log, Config: ac, GoalState: gs, ArtifactVersion: noneNPDVersion(cfg),
		}).GoalState()
	}
	return ac, gs, archives, err
}

func setupNoneHost(cfg *config.Config, log *slog.Logger) phases.Task {
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

func startNoneNode(cfg *config.Config, log *slog.Logger, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging, store stateStore, state *State) phases.Task {
	return hostnode.Func("start-none-node", func(ctx context.Context) error {
		ac := config.ToAgentConfig(cfg, noneops.Slot)
		registration := *state
		registration.NodeConfig = ac
		ac = noneRegistrationState(&registration).NodeConfig
		gs.NodeStart.Kubelet.NodeLabels = ac.Kubelet.Labels
		node := noneops.New(noneops.Params{
			Log: log, Config: ac, GoalState: gs, Generation: state.AppliedSettingsVersion,
			ArtifactVersion: noneNPDVersion(cfg), Install: noneArtifacts(cfg, log),
		})
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
		if err := node.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
			return err
		}
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		if err := npd.Start(log, cfg, gs.NodeStart).Do(ctx); err != nil {
			return err
		}
		seedNoneState(state, ac, noneNPDVersion(cfg))
		return saveState(store, state).Do(ctx)
	})
}

func resetNoneNode(log *slog.Logger, teardown *noneops.Teardown) phases.Task {
	return phases.Serial(log,
		removeNoneHostUnit(log, npd.HostUnitNPD),
		teardown.Stop(),
		teardown.Destroy(),
		teardown.RemoveConfig(),
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
