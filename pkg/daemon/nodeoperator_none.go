package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

// The SDK journals host changes and rolls back failures. Flex persists the
// applied configuration before committing that journal.
type noneNode interface {
	RecoverUpgrade(context.Context) error
	Restart(context.Context) error
	Upgrade(context.Context) error
	Commit() error
	PruneVersions() error
	GoalState() (*goalstates.MachineGoalState, error)
}

type noneNodeOperator struct {
	*nspawnNodeOperator
	newNode      func(noneops.Params) noneNode
	stage        func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error
	dependencies func(context.Context, *slog.Logger, *config.Config) error
	installNPD   func(context.Context, *slog.Logger, *config.Config, *goalstates.MachineGoalState) error
	startNPD     func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error
	resolve      func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	// Reconciliation and MachineOperations share a serialized queue.
	recovered bool
}

func newNodeOperator(ctx context.Context, cfg *config.Config, store stateStore) (nodeOperator, error) {
	if err := ValidateIsolationEnvironment(cfg); err != nil {
		return nil, err
	}
	base, err := newNSpawnNodeOperator(cfg, store)
	if err != nil {
		return nil, err
	}
	state, err := store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state != nil && state.isolationMode() != cfg.Bootstrap.Isolation {
		return nil, fmt.Errorf("configured isolation %s differs from applied isolation %s; reset the node before changing modes", cfg.Bootstrap.Isolation, state.isolationMode())
	}
	switch cfg.Bootstrap.Isolation {
	case isolation.ModeNSpawn:
		return base, nil
	case isolation.ModeContainer:
		return newContainerNodeOperator(base, state)
	}
	if state == nil || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return nil, fmt.Errorf("none node has no valid applied configuration")
	}
	return &noneNodeOperator{
		nspawnNodeOperator: base,
		newNode:            func(p noneops.Params) noneNode { return newNoneRuntime(p) },
		resolve:            config.ResolveMachineGoalState,
		dependencies:       prepareNoneDependencies,
		installNPD:         installNoneNPD,
		stage: func(ctx context.Context, log *slog.Logger, archives *goalstates.ContainerImageArchiveStaging) error {
			return stageContainerImageArchiveBindSource(log, archives).Do(ctx)
		},
		startNPD: func(ctx context.Context, log *slog.Logger, cfg *config.Config, ns *goalstates.NodeStart) error {
			return npd.Restart(log, cfg, ns).Do(ctx)
		},
	}, nil
}

func (o *noneNodeOperator) applied(ctx context.Context) (*State, error) {
	state, err := o.state.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state == nil || state.isolationMode() != isolation.ModeNone || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return nil, fmt.Errorf("none node has no applied configuration")
	}
	return state, nil
}

func (o *noneNodeOperator) recover(ctx context.Context, log *slog.Logger) error {
	if o.recovered {
		return nil
	}
	state, err := o.applied(ctx)
	if err != nil {
		return err
	}
	if err := o.dependencies(ctx, log, o.cfg); err != nil {
		return err
	}
	// Do not stamp a new nonce here: the SDK compares the applied config's
	// digest with its journal, including for settings-only repaves.
	node := o.newNode(noneops.Params{Log: log, Config: state.NodeConfig})
	if err := node.RecoverUpgrade(ctx); err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	cfg := noneNPDConfig(state)
	if err := o.installNPD(ctx, log, cfg, gs); err != nil {
		return err
	}
	if err := o.startNPD(ctx, log, cfg, gs.NodeStart); err != nil {
		return err
	}
	o.recovered = true
	return nil
}

func (o *noneNodeOperator) RestartNode(ctx context.Context, log *slog.Logger) error {
	if err := o.recover(ctx, log); err != nil {
		return fmt.Errorf("recover none node before restart: %w", err)
	}
	state, err := o.applied(ctx)
	if err != nil {
		return err
	}
	node := o.newNode(noneops.Params{Log: log, Config: state.NodeConfig})
	if err := node.Restart(ctx); err != nil {
		return fmt.Errorf("restart none node: %w", err)
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return o.startNPD(ctx, log, noneNPDConfig(state), gs.NodeStart)
}

func (o *noneNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error) {
	if err := o.recover(ctx, log); err != nil {
		return nil, fmt.Errorf("recover none node before repave: %w", err)
	}
	state, err := o.applied(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := o.configForGoalState(ctx, log, goal)
	if err != nil {
		return nil, err
	}
	ac, gs, archives, err := o.resolve(ctx, log, cfg, noneops.Slot)
	if err != nil {
		return nil, err
	}
	next := nextAppliedState(state, goal, nil)
	next.Isolation, next.NodeConfig, next.NPDVersion = isolation.ModeNone, ac, noneNPDVersion(cfg)
	next = noneRegistrationState(next)
	ac = next.NodeConfig
	gs.NodeStart.Kubelet.NodeLabels = ac.Kubelet.Labels
	node := o.newNode(noneops.Params{
		Log: log, Config: ac, GoalState: gs, Previous: state.NodeConfig,
	})
	if err := o.stage(ctx, log, archives); err != nil {
		return nil, err
	}
	// Upgrade and RecoverUpgrade can start kubelet internally.
	if err := o.dependencies(ctx, log, cfg); err != nil {
		return nil, err
	}
	o.recovered = false
	if err := node.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade none node: %w", err)
	}
	gs, err = node.GoalState()
	if err != nil {
		return nil, err
	}
	if err := o.installNPD(ctx, log, noneNPDConfig(next), gs); err != nil {
		return nil, err
	}
	if err := o.startNPD(ctx, log, noneNPDConfig(next), gs.NodeStart); err != nil {
		return nil, err
	}
	// Until this save succeeds, recovery restores the source configuration.
	if err := o.state.Save(ctx, next); err != nil {
		return nil, fmt.Errorf("persist none node: %w", err)
	}
	if err := node.Commit(); err != nil {
		log.Warn("none upgrade cleanup will be retried at startup", "error", err)
		return next, nil
	}
	o.recovered = true
	if err := node.PruneVersions(); err != nil {
		log.Warn("could not prune previous node binaries", "error", err)
	}
	return next, nil
}

func seedNoneState(state *State, cfg *agentconfig.AgentConfig, npdVersion string) {
	state.Isolation = isolation.ModeNone
	state.ActiveMachine = ""
	state.NodeConfig = cfg
	state.NPDVersion = npdVersion
}
