package provider

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

var _ NodeProvider = None{}

// None implements the host-native worker lifecycle.
type None struct {
	Host            HostServices
	OperatorFactory OperatorFactory
}

func (None) Mode() isolation.Mode {
	return isolation.ModeNone
}

func (None) RuntimeChecks(cfg *config.Config, log *slog.Logger, containerized bool) []preflight.Checker {
	return nil
}

func (None) WantedTargets() []string {
	return []string{"network-online.target", "systemd-udev-settle.service"}
}

func (None) SupportsDisconnectTolerance() bool {
	return false
}

func (None) NSpawnRuntime() NSpawnRuntime {
	return nil
}

func (None) ValidateAgentActivation(ctx context.Context) error {
	return nil
}

func (None) ValidateActiveSlot(slot string) error {
	return nil
}

func (None) SynchronizeAgent(source string, slot string) error {
	return nil
}

func (None) ValidateAgent(source string, slot string) error {
	return nil
}

func (None) RollbackAgent(source string, slot string) error {
	return nil
}

func (None) CheckAgentCandidate(ctx context.Context, candidate string) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, candidate, "supports-isolation", string(isolation.ModeNone)).Run(); err != nil { //nolint:gosec // candidate is verified before this capability probe
		return fmt.Errorf("candidate agent cannot operate isolation mode none: %w", err)
	}
	return nil
}

func (None) ValidateResetState(state *State) error {
	if state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return fmt.Errorf("none node has no valid applied configuration for reset")
	}
	return nil
}

func (p None) ResetNode(log *slog.Logger) phases.Task {
	return phases.Serial(log,
		RemoveHostUnit(log, "node-problem-detector.service"),
		ResetNoneRuntime(log, &noneops.Teardown{Log: log}, hostnode.ResetCNIState),
		reset.CleanupNetwork(log),
		reset.RemoveWireGuardKeys(log),
		reset.RemoveNFTablesFlush(log),
		p.Host.RemoveHostHelpers(log),
		RemoveHostUnit(log, "static-routes.service"),
		RemoveHostUnit(log, "check-route-overlap.service"),
		hostnode.Func("remove-host-npd-config", func(ctx context.Context) error {
			return p.Host.RemoveIfPresent("/etc/node-problem-detector/kernel-monitor.json")
		}),
		reset.ReloadSystemd(log),
		config.RemoveRuntimeDirs(log),
	)
}

func (p None) RewriteNodeUnits(ctx context.Context, log *slog.Logger, cfg *config.Config, state StateStore) error {
	applied, err := state.Load(ctx)
	if err != nil {
		return err
	}
	if applied == nil || applied.NodeConfig == nil {
		return fmt.Errorf("none node has no applied configuration")
	}
	gs, err := NewNoneRuntime(noneops.Params{Log: log, Config: applied.NodeConfig}).GoalState()
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
	return p.Host.SyncDirs(
		goalstates.SystemdSystemDir,
		filepath.Dir(p.Host.RecoveryScriptPathUnder(hostroot.Resolve())),
	)
}

func (None) ValidateEnvironment(containerized bool) error {
	if containerized {
		return fmt.Errorf("none isolation requires the host-installed agent; the containerized agent requires host containerd")
	}
	return nil
}

func (None) BootstrapPresent(log *slog.Logger) bool {
	return (&noneops.Teardown{Log: log}).Present()
}

func (p None) CheckBootstrap(ctx context.Context, log *slog.Logger, applied *State) error {
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

func (None) ResolveGoalState(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error) {
	ac, gs, archives, err := config.ResolveMachineGoalState(ctx, log, cfg, slot)
	if err != nil {
		return nil, nil, nil, err
	}
	gs, err = NewNoneRuntime(noneops.Params{Log: log, Config: ac, GoalState: gs}).GoalState()
	return ac, gs, archives, err
}

func (None) SetupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	return phases.Serial(log,
		host.InstallPackagesFor(log, isolation.ModeNone),
		noneops.LoadKernelModules(log, nil),
		phases.Parallel(log,
			host.ConfigureOS(log),
			host.ConfigureNFTablesFor(log, isolation.ModeNone),
			host.DisableDocker(log),
			host.DisableSwap(log),
			host.RemoveAPTHardening(log),
			hostrouting.Configure(cfg, log),
		),
	)
}

func (p None) StartNode(params StartParams) phases.Task {
	cfg, log, gs := params.Config, params.Log, params.GoalState
	return hostnode.Func("start-none-node", func(ctx context.Context) error {
		ac := config.ToAgentConfig(cfg, noneops.Slot).DeepCopy()
		node := NewNoneRuntime(noneops.Params{Log: log, Config: ac, GoalState: gs})
		if err := StageContainerImageArchiveBindSource(log, params.ContainerImageArchives).Do(ctx); err != nil {
			return err
		}
		if err := node.Provision(ctx); err != nil {
			return err
		}
		containerdChanged, kubeletChanged, err := node.Configure(ctx)
		if err != nil {
			return err
		}
		if err := PrepareNoneDependencies(ctx, log, cfg); err != nil {
			return err
		}
		if err := node.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
			return err
		}
		gs, err = node.GoalState()
		if err != nil {
			return err
		}
		if err := InstallNoneNPD(ctx, log, cfg, gs); err != nil {
			return err
		}
		if err := npd.Start(log, cfg, gs.NodeStart.Root(), gs.NodeStart, isolation.ModeNone).Do(ctx); err != nil {
			return err
		}
		SeedNoneState(params.State, ac)
		return p.Host.SaveState(params.Store, params.State).Do(ctx)
	})
}

func (p None) NewOperator(ctx context.Context, cfg *config.Config, store StateStore) (Operator, error) {
	return p.OperatorFactory(ctx, cfg, store, p)
}

// NewNoneRuntime keeps SDK recovery files within Flex's reset ownership.
func NewNoneRuntime(params noneops.Params) *noneops.Node {
	params.ConfigSnapshotDir = filepath.Join(params.HostRoot, config.ConfigDir, "none-upgrade-config")
	return noneops.New(params)
}

// SeedNoneState records the host-native applied configuration.
func SeedNoneState(state *State, cfg *agentconfig.AgentConfig) {
	state.Isolation, state.ActiveMachine, state.NodeConfig = isolation.ModeNone, "", cfg
}

// ResetNoneRuntime ends runtime processes before reclaiming host CNI/IPAM state.
func ResetNoneRuntime(log *slog.Logger, teardown *noneops.Teardown, resetCNI func(root string) error) phases.Task {
	return phases.Serial(log,
		teardown.Stop(),
		teardown.Destroy(),
		hostnode.Func("remove-none-cni-state", func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := resetCNI(teardown.Root); err != nil {
				return fmt.Errorf("reset none CNI state: %w", err)
			}
			return nil
		}),
		teardown.RemoveConfig(),
	)
}

// RemoveHostUnit disables an owned host service before removing its unit file.
func RemoveHostUnit(log *slog.Logger, unit string) phases.Task {
	return hostnode.Func("remove-"+unit, func(ctx context.Context) error {
		path := filepath.Join(goalstates.SystemdSystemDir, unit)
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

// NoneRequiredServices returns host services needed before worker startup.
func NoneRequiredServices(cfg *config.Config) []string {
	services := append([]string{"check-route-overlap.service"}, cfg.Bootstrap.AdditionalRequiredServices...)
	if len(cfg.HostRouting.StaticRoutes.Routes) > 0 {
		services = append(services, "static-routes.service")
	}
	return services
}

// ConfigureNoneDependencies writes runtime unit ordering against host services.
func ConfigureNoneDependencies(log *slog.Logger, services []string) phases.Task {
	return hostnode.Func("configure-none-dependencies", func(ctx context.Context) error {
		units := strings.Join(services, " ")
		content := []byte("[Unit]\nRequires=" + units + "\nAfter=" + units + "\n")
		for _, unit := range []string{none.UnitContainerd, none.UnitKubelet, none.UnitLocalDNS} {
			if err := utilio.WriteFile(filepath.Join(goalstates.SystemdSystemDir, unit+".d", "10-flex-dependencies.conf"), content, 0o644); err != nil {
				return err
			}
		}
		return utilexec.ReloadSystemd(ctx, log)
	})
}

// PrepareNoneDependencies converges host-native unit dependencies before the SDK starts kubelet.
func PrepareNoneDependencies(ctx context.Context, log *slog.Logger, cfg *config.Config) error {
	return ConfigureNoneDependencies(log, NoneRequiredServices(cfg)).Do(ctx)
}

// InstallNoneNPD installs NPD into the SDK-selected runtime root.
func InstallNoneNPD(ctx context.Context, log *slog.Logger, cfg *config.Config, gs *goalstates.MachineGoalState) error {
	return npd.Download(log, cfg, gs.RootFS.Root()).Do(ctx)
}
