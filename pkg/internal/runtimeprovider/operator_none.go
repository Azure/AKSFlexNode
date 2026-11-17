package runtimeprovider

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
	corev1 "k8s.io/api/core/v1"
)

// The SDK journals host changes and rolls back failures. Flex persists the
// applied configuration before committing that journal.
type noneNode interface {
	RecoverUpgrade(ctx context.Context) error
	Restart(ctx context.Context) error
	Upgrade(ctx context.Context) error
	Commit() error
	PruneVersions() error
	GoalState() (*goalstates.MachineGoalState, error)
}

type noneNodeOperator struct {
	*operatorBase
	newNode      func(params noneops.Params) noneNode
	stage        func(ctx context.Context, log *slog.Logger, archives *goalstates.ContainerImageArchiveStaging) error
	dependencies func(ctx context.Context, log *slog.Logger, cfg *config.Config) error
	installNPD   func(ctx context.Context, log *slog.Logger, cfg *config.Config, goal *goalstates.MachineGoalState) error
	startNPD     func(ctx context.Context, log *slog.Logger, cfg *config.Config, node *goalstates.NodeStart) error
	resolve      func(ctx context.Context, log *slog.Logger, cfg *config.Config, slot string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	// Reconciliation and MachineOperations share a serialized queue.
	recovered bool
}

func newNoneOperator(ctx context.Context, cfg *config.Config, store StateStore, worker NodeProvider, host HostServices) (Operator, error) {
	base, state, err := newOperatorBase(ctx, cfg, store, worker, host)
	if err != nil {
		return nil, err
	}
	if state == nil || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return nil, fmt.Errorf("none node has no valid applied configuration")
	}
	return &noneNodeOperator{
		operatorBase: base,
		newNode:      func(p noneops.Params) noneNode { return NewNoneRuntime(p) },
		resolve:      config.ResolveMachineGoalState,
		dependencies: PrepareNoneDependencies,
		installNPD:   InstallNoneNPD,
		stage: func(ctx context.Context, log *slog.Logger, archives *goalstates.ContainerImageArchiveStaging) error {
			return StageContainerImageArchiveBindSource(log, archives).Do(ctx)
		},
		startNPD: func(ctx context.Context, log *slog.Logger, cfg *config.Config, ns *goalstates.NodeStart) error {
			return npd.Start(log, cfg, ns.Root(), ns, isolation.ModeNone).Do(ctx)
		},
	}, nil
}

func (o *noneNodeOperator) applied(ctx context.Context) (*State, error) {
	state, err := o.state.Load(ctx)
	if err != nil {
		return nil, err
	}
	if state == nil || state.Isolation != isolation.ModeNone || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
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
	// The SDK compares the applied config's digest with its journal.
	node := o.newNode(noneops.Params{Log: log, Config: state.NodeConfig})
	if err := node.RecoverUpgrade(ctx); err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	if err := o.installNPD(ctx, log, o.cfg, gs); err != nil {
		return err
	}
	if err := o.startNPD(ctx, log, o.cfg, gs.NodeStart); err != nil {
		return err
	}
	o.recovered = true
	return nil
}

// ResumeRepave runs the SDK's applied-config recovery before normal RP
// reconciliation. Recovery may roll an uncommitted target back to the source.
func (o *noneNodeOperator) ResumeRepave(ctx context.Context, log *slog.Logger, _ *corev1.Node) (bool, error) {
	return false, o.recover(ctx, log)
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
	return o.startNPD(ctx, log, o.cfg, gs.NodeStart)
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
	next.Isolation, next.NodeConfig = isolation.ModeNone, ac
	next = next.DeepCopy()
	ac = next.NodeConfig
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
	if err := o.installNPD(ctx, log, cfg, gs); err != nil {
		return nil, err
	}
	if err := o.startNPD(ctx, log, cfg, gs.NodeStart); err != nil {
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
