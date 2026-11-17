package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	runtimeprovider "github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	corev1 "k8s.io/api/core/v1"
)

type containerNode interface {
	runtimeprovider.ContainerRuntime
	Start(context.Context, bool, bool, bool) error
	Restart(context.Context) error
	Destroy(context.Context) error
	Upgrade(context.Context, containerNode) error
}

type managedContainerNode struct{ *containerops.Node }

func (n managedContainerNode) Upgrade(ctx context.Context, old containerNode) error {
	source, ok := old.(managedContainerNode)
	if !ok {
		return fmt.Errorf("invalid source container operations")
	}
	return n.Node.Upgrade(ctx, source.Node)
}

type containerNodeOperator struct {
	*operatorBase
	startup            containerStartupStore
	activationLockPath string
	newNode            func(containerops.Params) (containerNode, error)
	resolve            func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	stage              func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error
	startNPD           func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error
	syncPrepared       func(context.Context, runtimeprovider.ContainerRuntime) error
}

func newContainerNodeOperator(base *operatorBase, state *runtimeprovider.State, startup containerStartupStore, activationLockPath string) (*containerNodeOperator, error) {
	if !runtimeprovider.ValidContainerState(state) {
		return nil, fmt.Errorf("container node has no valid applied configuration or slot")
	}
	return &containerNodeOperator{
		operatorBase: base, startup: startup, activationLockPath: activationLockPath,
		newNode: func(p containerops.Params) (containerNode, error) {
			p.HelperPath = config.HostBinaryPath()
			p.ConfigSnapshotDir = filepath.Join(config.ConfigDir, "container-upgrade-config")
			node, err := runtimeprovider.NewContainerNode(p, base.cfg)
			if err != nil {
				return nil, err
			}
			return managedContainerNode{node}, nil
		},
		resolve:      config.ResolveMachineGoalState,
		syncPrepared: runtimeprovider.SyncContainerFilesystems,
		stage: func(ctx context.Context, log *slog.Logger, archives *goalstates.ContainerImageArchiveStaging) error {
			return runtimeprovider.StageContainerImageArchiveBindSource(log, archives).Do(ctx)
		},
		startNPD: func(ctx context.Context, log *slog.Logger, cfg *config.Config, ns *goalstates.NodeStart) error {
			return npd.Start(log, cfg, ns.Root(), ns).Do(ctx)
		},
	}, nil
}

func (o *containerNodeOperator) applied(ctx context.Context) (*runtimeprovider.State, error) {
	state, err := o.state.Load(ctx)
	if err != nil {
		return nil, err
	}
	if !runtimeprovider.ValidContainerState(state) {
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
		return fmt.Errorf("container startup must complete before another operation")
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
	node, err := o.newNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	if err != nil {
		return err
	}
	if err := node.Restart(ctx); err != nil {
		return fmt.Errorf("restart container node: %w", err)
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	return o.startNPD(ctx, log, o.cfg, gs.NodeStart)
}

func (o *containerNodeOperator) ApplyGoalState(ctx context.Context, log *slog.Logger, goal aksmachine.GoalState) (*runtimeprovider.State, error) {
	unlock, err := o.lockAgentActivation(log)
	if err != nil {
		return nil, err
	}
	defer unlock()
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
	next.Isolation, next.NodeConfig = isolation.ModeContainer, ac
	next = next.DeepCopy()
	node, err := o.newNode(containerops.Params{Log: log, Config: next.NodeConfig, GoalState: gs, Slot: slot})
	if err != nil {
		return nil, err
	}
	node.Backend().UnitRequires = append(node.Backend().UnitRequires, runtimeprovider.NoneRequiredServices(cfg)...)
	old, err := o.newNode(containerops.Params{Log: log, Config: state.NodeConfig, Slot: state.ActiveMachine})
	if err != nil {
		return nil, err
	}
	if err := o.stage(ctx, log, archives); err != nil {
		return nil, err
	}
	startup := &containerStartup{Phase: "preparing", Next: next, Previous: state}
	if err := o.startup.Save(ctx, startup); err != nil {
		return nil, err
	}
	// Upgrade owns preparation, activation and rollback on failure. A crash or
	// error leaves preparing fenced: it cannot establish which slot is safe.
	if err := node.Upgrade(ctx, old); err != nil {
		return nil, fmt.Errorf("upgrade container node: %w", err)
	}
	// The SDK has already started the target and discarded its shared snapshot.
	// Persist that target before journaling Flex's remaining completion work.
	if err := o.syncPrepared(ctx, node); err != nil {
		return nil, fmt.Errorf("persist upgraded container before completion journal: %w", err)
	}
	startup.Phase = "starting"
	if err := o.startup.Save(ctx, startup); err != nil {
		return nil, err
	}
	if err := o.finishStartup(ctx, log, startup); err != nil {
		return nil, err
	}
	return next, nil
}

// ResumeRepave retries Flex completion only after a recorded successful SDK
// upgrade. It never infers recovery from the slots left by a failed upgrade.
func (o *containerNodeOperator) ResumeRepave(ctx context.Context, log *slog.Logger, _ *corev1.Node) (bool, error) {
	startup, err := o.startup.Load()
	if err != nil || startup == nil {
		return err != nil, err
	}
	unlock, err := o.lockAgentActivation(log)
	if err != nil {
		return true, err
	}
	defer unlock()
	state, err := o.applied(ctx)
	if err != nil {
		return true, err
	}
	if !reflect.DeepEqual(state, startup.Previous) && !reflect.DeepEqual(state, startup.Next) {
		return true, fmt.Errorf("container startup does not match applied source or target state")
	}
	if startup.Phase == "preparing" {
		return true, fmt.Errorf("container SDK upgrade did not record successful completion; operator recovery or reset required")
	}
	if startup.Phase == "starting" {
		node, err := o.newNode(containerops.Params{Log: log, Config: startup.Next.NodeConfig, Slot: startup.Next.ActiveMachine})
		if err != nil {
			return true, err
		}
		if err := node.Start(ctx, false, false, false); err != nil {
			return true, fmt.Errorf("start upgraded container for completion: %w", err)
		}
		if err := o.finishStartup(ctx, log, startup); err != nil {
			return true, err
		}
	}
	return true, o.startup.Delete()
}

// Direct agent activation is a separate process. Share its nonblocking lock so
// it cannot pass a startup check immediately before a repave publishes intent.
func (o *containerNodeOperator) lockAgentActivation(log *slog.Logger) (func(), error) {
	lock, err := agentbinary.AcquireHostActivationLock(o.activationLockPath)
	if err != nil {
		return nil, fmt.Errorf("serialize container repave with agent activation: %w", err)
	}
	return func() {
		if err := lock.Close(); err != nil {
			log.Warn("release container repave activation lock", "error", err)
		}
	}, nil
}

func (o *containerNodeOperator) finishStartup(ctx context.Context, log *slog.Logger, startup *containerStartup) error {
	node, err := o.newNode(containerops.Params{Log: log, Config: startup.Next.NodeConfig, Slot: startup.Next.ActiveMachine})
	if err != nil {
		return err
	}
	gs, err := node.GoalState()
	if err != nil {
		return err
	}
	if err := o.startNPD(ctx, log, o.cfg, gs.NodeStart); err != nil {
		return err
	}
	// Persist the supervisor and NPD boot state before recording Flex completion
	// or retiring the stopped source. The SDK's rollback snapshot is already gone.
	if err := o.syncPrepared(ctx, node); err != nil {
		return fmt.Errorf("persist started container before completion: %w", err)
	}
	if err := o.state.Save(ctx, startup.Next); err != nil {
		return fmt.Errorf("persist frozen container state: %w", err)
	}
	old, err := o.newNode(containerops.Params{Log: log, Config: startup.Previous.NodeConfig, Slot: startup.Previous.ActiveMachine})
	if err != nil {
		return err
	}
	if err := old.Destroy(ctx); err != nil {
		return fmt.Errorf("retire source container slot: %w", err)
	}
	startup.Phase = "finished"
	return o.startup.Save(ctx, startup)
}
