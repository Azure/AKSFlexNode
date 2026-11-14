package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	corev1 "k8s.io/api/core/v1"
)

type containerNode interface {
	Start(context.Context, bool, bool, bool) error
	Restart(context.Context) error
	Destroy(context.Context) error
	CommitUpgrade() error
	UpgradeWithStartup(context.Context, containerNode, func(context.Context) error) error
	GoalState() (*goalstates.MachineGoalState, error)
	Backend() *container.Backend
}

type managedContainerNode struct{ *containerops.Node }

func (n managedContainerNode) UpgradeWithStartup(ctx context.Context, old containerNode, publish func(context.Context) error) error {
	source, ok := old.(managedContainerNode)
	if !ok {
		return fmt.Errorf("invalid source container operations")
	}
	return n.Node.UpgradeWithStartup(ctx, source.Node, publish)
}

type containerNodeOperator struct {
	*nspawnNodeOperator
	startup  containerStartupStore
	newNode  func(containerops.Params) containerNode
	resolve  func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	stage    func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error
	startNPD func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error
}

func validContainerState(state *State) bool {
	return state != nil && state.isolationMode() == isolation.ModeContainer && validActiveMachine(state.ActiveMachine) &&
		state.NodeConfig != nil && state.NodeConfig.Isolation == string(isolation.ModeContainer) && state.NodeConfig.MachineName == state.ActiveMachine &&
		state.NodeConfig.Experimental != nil && state.NodeConfig.Experimental.ContainerIsolation
}

func newContainerNodeOperator(base *nspawnNodeOperator, state *State) (*containerNodeOperator, error) {
	if !validContainerState(state) {
		return nil, fmt.Errorf("container node has no valid applied configuration or slot")
	}
	return &containerNodeOperator{
		nspawnNodeOperator: base, startup: defaultContainerStartupStore(),
		newNode: func(p containerops.Params) containerNode { return managedContainerNode{newContainerNode(p)} },
		resolve: config.ResolveMachineGoalState,
		stage: func(ctx context.Context, log *slog.Logger, archives *goalstates.ContainerImageArchiveStaging) error {
			return stageContainerImageArchiveBindSource(log, archives).Do(ctx)
		},
		startNPD: func(ctx context.Context, log *slog.Logger, cfg *config.Config, ns *goalstates.NodeStart) error {
			return npd.Restart(log, cfg, ns).Do(ctx)
		},
	}, nil
}

func (o *containerNodeOperator) applied(ctx context.Context) (*State, error) {
	state, err := o.state.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !validContainerState(state) {
		return nil, fmt.Errorf("container node has no valid applied configuration or slot")
	}
	return state, nil
}

func (o *containerNodeOperator) requireNoStartup() error {
	startup, err := o.startup.Load()
	if err != nil {
		return err
	}
	if startup != nil {
		return fmt.Errorf("container startup must complete and its registration be observed before another operation")
	}
	return nil
}

func (o *containerNodeOperator) RestartNode(ctx context.Context, log *slog.Logger) error {
	if err := o.requireNoStartup(); err != nil {
		return err
	}
	state, err := o.applied(ctx)
	if err != nil {
		return err
	}
	node := o.newNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	if err := node.Restart(ctx); err != nil {
		return fmt.Errorf("restart container node: %w", err)
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return o.startNPD(ctx, log, noneNPDConfig(state), gs.NodeStart)
}

func (o *containerNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*State, error) {
	if err := o.requireNoStartup(); err != nil {
		return nil, err
	}
	state, err := o.applied(ctx)
	if err != nil {
		return nil, err
	}
	cfg, err := o.configForGoalState(ctx, log, goal)
	if err != nil {
		return nil, err
	}
	slot := container.OtherSlot(state.ActiveMachine)
	ac, gs, archives, err := o.resolve(ctx, log, cfg, slot)
	if err != nil {
		return nil, fmt.Errorf("resolve container repave: %w", err)
	}
	next := nextAppliedState(state, goal, &activeMachine{Name: slot})
	next.Isolation, next.NodeConfig, next.NPDVersion = isolation.ModeContainer, ac, noneNPDVersion(cfg)
	next = noneRegistrationState(next)
	gs.NodeStart.Kubelet.NodeLabels = next.NodeConfig.Kubelet.Labels
	node := o.newNode(containerops.Params{Log: log, Config: next.NodeConfig, GoalState: gs, Slot: slot, Install: containerArtifacts(cfg, log)})
	if _, err := node.GoalState(); err != nil {
		return nil, err
	}
	node.Backend().UnitRequires = append(node.Backend().UnitRequires, noneRequiredServices(cfg)...)
	old := o.newNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	if err := o.stage(ctx, log, archives); err != nil {
		return nil, err
	}
	startup := &containerStartup{Phase: "preparing", Next: next, Previous: state}
	if err := o.startup.Save(ctx, startup); err != nil {
		return nil, err
	}
	// The shared recipe prepares beside the source, stops it, resets shared
	// identity and configures the target. Publication precedes all exposure of
	// target kubelet, including the supervising unit's post-start hook.
	if err := node.UpgradeWithStartup(ctx, old, func(ctx context.Context) error {
		startup.Phase = "starting"
		return o.startup.Save(ctx, startup)
	}); err != nil {
		return nil, fmt.Errorf("upgrade container node: %w", err)
	}
	if err := o.finishStartup(ctx, log, startup); err != nil {
		return nil, err
	}
	return next, nil
}

// ResumeRepave never resolves a newer goal or rewrites shared configuration.
// A published startup may safely retry Start even after the Node has appeared.
func (o *containerNodeOperator) ResumeRepave(ctx context.Context, log *slog.Logger, observed *corev1.Node) (bool, error) {
	startup, err := o.startup.Load()
	if err != nil || startup == nil {
		return err != nil, err
	}
	state, err := o.applied(ctx)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(state, startup.Previous) && !reflect.DeepEqual(state, startup.Next) {
		return true, fmt.Errorf("container startup does not match applied source or target state")
	}
	if startup.Phase == "preparing" {
		return true, fmt.Errorf("interrupted container preparation requires operator recovery or reset; source and snapshot retained")
	}
	if startup.Phase == "starting" {
		node := o.newNode(containerops.Params{Log: log, Config: startup.Next.NodeConfig, Slot: startup.Next.ActiveMachine})
		if err := node.Start(ctx, false, false, false); err != nil {
			return true, fmt.Errorf("resume frozen container startup: %w", err)
		}
		if err := o.finishStartup(ctx, log, startup); err != nil {
			return true, err
		}
	}
	expected := startup.Next.NodeConfig.Kubelet.Labels[noneGenerationLabel]
	if observed != nil && observed.Labels[noneGenerationLabel] == expected {
		if err := o.startup.Delete(); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (o *containerNodeOperator) finishStartup(ctx context.Context, log *slog.Logger, startup *containerStartup) error {
	node := o.newNode(containerops.Params{Log: log, Config: startup.Next.NodeConfig, Slot: startup.Next.ActiveMachine})
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	if err := o.startNPD(ctx, log, noneNPDConfig(startup.Next), gs.NodeStart); err != nil {
		return err
	}
	if err := o.state.Save(ctx, startup.Next); err != nil {
		return fmt.Errorf("persist frozen container state: %w", err)
	}
	old := o.newNode(containerops.Params{Log: log, Config: startup.Previous.NodeConfig, Slot: startup.Previous.ActiveMachine})
	if err := old.Destroy(ctx); err != nil {
		return fmt.Errorf("retire source container slot: %w", err)
	}
	if err := node.CommitUpgrade(); err != nil {
		return err
	}
	startup.Phase = "finished"
	return o.startup.Save(ctx, startup)
}
