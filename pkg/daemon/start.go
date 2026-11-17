package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/AKSFlexNode/pkg/hostrouting"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
)

func SetupHost(cfg *config.Config, log *slog.Logger) phases.Task {
	return setupHost(cfg, log, Containerized())
}
func setupHost(cfg *config.Config, log *slog.Logger, containerized bool) phases.Task {
	mode := cfg.Bootstrap.Isolation
	var tasks []phases.Task
	if !containerized {
		tasks = append(tasks, host.InstallPackagesFor(log, mode))
	}
	if mode != isolation.ModeNSpawn {
		tasks = append(tasks, noneops.LoadKernelModules(log, nil))
	}
	if mode == isolation.ModeContainer {
		tasks = append(tasks, phases.Parallel(log, host.DisableContainerd(log), host.DisableKubelet(log)))
	}
	preparation := []phases.Task{host.ConfigureOS(log), host.ConfigureNFTablesFor(log, mode), host.DisableDocker(log), host.DisableSwap(log), hostrouting.Configure(cfg, log)}
	if mode == isolation.ModeNSpawn {
		preparation = append(preparation, host.HardenAPT(log))
	} else {
		preparation = append(preparation, host.RemoveAPTHardening(log))
	}
	return phases.Serial(log, append(tasks, phases.Parallel(log, preparation...))...)
}

// StartNode keeps daemon persistence outside the worker implementation.
func StartNode(cfg *config.Config, log *slog.Logger, machineName string, gs *goalstates.MachineGoalState, archives *goalstates.ContainerImageArchiveStaging, store stateStore, state *State) (phases.Task, error) {
	runtime, err := provider.New(cfg.Bootstrap.Isolation)
	if err != nil {
		return nil, err
	}
	var tasks []phases.Task
	if cfg.Bootstrap.Isolation == isolation.ModeNSpawn {
		// The machine hooks invoke agent-owned lifecycle units.
		tasks = append(tasks, EnsureNSpawnLifecycleUnits(log))
	}
	tasks = append(tasks, &startWorkerTask{log: log, runtime: runtime, params: provider.StartParams{
		Node: provider.Node{Slot: machineName, Config: state.NodeConfig, GoalState: gs}, Config: cfg, Archives: archives,
	}, store: store, state: state})
	return phases.Serial(log, tasks...), nil
}

type startWorkerTask struct {
	log     *slog.Logger
	runtime provider.Provider
	params  provider.StartParams
	store   stateStore
	state   *State
}

func (*startWorkerTask) Name() string { return "start-worker" }
func (t *startWorkerTask) Do(ctx context.Context) error {
	persist, err := t.runtime.Start(ctx, t.log, t.params)
	if err != nil {
		return err
	}
	if !persist {
		applied, err := t.store.Load(ctx)
		if err != nil {
			return err
		}
		if applied == nil {
			return fmt.Errorf("running worker has no applied configuration; reset before bootstrap")
		}
		return nil
	}
	return saveState(t.store, t.state).Do(ctx)
}
