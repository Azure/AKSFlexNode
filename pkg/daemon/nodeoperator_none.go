package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/noneops"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// The orchestration in Unbounded's cmd/agent/internal cannot be imported by
// an embedding agent. internal/noneops composes its public backend and phases
// with Flex-owned persistence; the AKS goal and credential refresh stay here.
type noneNode interface {
	PendingStartup() (*noneops.Startup, error)
	PendingUpgrade() (bool, error)
	ResumeStartup(context.Context) error
	FinishStartup() error
	ClearStartup() error
	RecoverUpgrade(context.Context) error
	Restart(context.Context) error
	Upgrade(context.Context) error
	Commit() error
	PruneVersions() error
	GoalState() (*goalstates.MachineGoalState, error)
}

type noneNodeOperator struct {
	*nspawnNodeOperator
	newNode  func(noneops.Params) noneNode
	stage    func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error
	startNPD func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error
	resolve  func(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
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
	if state != nil && state.isolationMode() != cfg.Bootstrap.IsolationMode() {
		return nil, fmt.Errorf("configured isolation %s differs from applied isolation %s; reset the node before changing modes", cfg.Bootstrap.IsolationMode(), state.isolationMode())
	}
	if cfg.Bootstrap.IsolationMode() == isolation.ModeNSpawn {
		return base, nil
	}
	if state == nil || state.NodeConfig == nil || state.NodeConfig.Isolation != string(isolation.ModeNone) {
		return nil, fmt.Errorf("none node has no valid applied configuration")
	}
	return &noneNodeOperator{
		nspawnNodeOperator: base,
		newNode:            func(p noneops.Params) noneNode { return noneops.New(p) },
		resolve:            config.ResolveMachineGoalState,
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
	if state == nil || state.isolationMode() != isolation.ModeNone || state.NodeConfig == nil {
		return nil, fmt.Errorf("none node has no applied configuration")
	}
	return state, nil
}

func (o *noneNodeOperator) recover(ctx context.Context, log *slog.Logger) error {
	state, err := o.applied(ctx)
	if err != nil {
		return err
	}
	completion, err := json.Marshal(state)
	if err != nil {
		return err
	}
	node := o.newNode(noneops.Params{
		Log: log, Config: state.NodeConfig, Generation: state.AppliedSettingsVersion,
		ArtifactVersion: state.NPDVersion, Completion: completion,
	})
	if err := node.RecoverUpgrade(ctx); err != nil {
		return err
	}
	if startup, err := node.PendingStartup(); err != nil || startup != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("none startup must finish reconciliation before another operation")
	}
	return nil
}

func (o *noneNodeOperator) RestartNode(ctx context.Context, log *slog.Logger) error {
	state, err := o.applied(ctx)
	if err != nil {
		return err
	}
	pendingNode := o.newNode(noneops.Params{Log: log, Config: state.NodeConfig,
		Generation: state.AppliedSettingsVersion, ArtifactVersion: state.NPDVersion})
	if startup, err := pendingNode.PendingStartup(); err != nil || startup != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("none startup must complete before soft reboot")
	}
	if pending, err := pendingNode.PendingUpgrade(); err != nil || pending {
		if err != nil {
			return err
		}
		return fmt.Errorf("none preparation must recover before soft reboot")
	}
	if err := o.recover(ctx, log); err != nil {
		return fmt.Errorf("recover none node before restart: %w", err)
	}
	state, err = o.applied(ctx)
	if err != nil {
		return err
	}
	node := o.newNode(noneops.Params{
		Log: log, Config: state.NodeConfig, Generation: state.AppliedSettingsVersion,
		ArtifactVersion: state.NPDVersion,
	})
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
	previous := noneRegistrationState(state)
	completion, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	previousCompletion, err := json.Marshal(previous)
	if err != nil {
		return nil, err
	}
	node := o.newNode(noneops.Params{
		Log: log, Config: ac, GoalState: gs, Previous: previous.NodeConfig,
		Generation: goal.SettingsVersion, Install: noneArtifacts(cfg, log),
		Stage:           func(ctx context.Context) error { return o.stage(ctx, log, archives) },
		ArtifactVersion: noneNPDVersion(cfg), PreviousArtifactVersion: state.NPDVersion,
		Completion: completion, PreviousCompletion: previousCompletion,
		PreviousGeneration: state.AppliedSettingsVersion,
	})
	if err := node.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade none node: %w", err)
	}
	gs, err = node.GoalState()
	if err != nil {
		return nil, err
	}
	if err := o.startNPD(ctx, log, noneNPDConfig(next), gs.NodeStart); err != nil {
		return nil, err
	}
	// Persistence precedes Commit: startup checks the applied generation and
	// config digest to distinguish a completed upgrade from an interrupted one.
	if err := o.state.Save(ctx, next); err != nil {
		return nil, fmt.Errorf("persist none node: %w", err)
	}
	if err := node.Commit(); err != nil {
		log.Warn("none upgrade cleanup will be retried at startup", "error", err)
		return next, nil
	}
	if err := node.FinishStartup(); err != nil {
		return next, fmt.Errorf("record none completion: %w", err)
	}
	if err := node.PruneVersions(); err != nil {
		log.Warn("could not prune previous node binaries", "error", err)
	}
	return next, nil
}

func noneArtifacts(cfg *config.Config, log *slog.Logger) func(context.Context, *goalstates.MachineGoalState) error {
	return func(ctx context.Context, gs *goalstates.MachineGoalState) error {
		return phases.Serial(log,
			npd.DownloadToRoot(log, cfg, gs.RootFS.Root()),
			configureNoneDependencies(log, noneRequiredServices(cfg)),
		).Do(ctx)
	}
}

func seedNoneState(state *State, cfg *agentconfig.AgentConfig, npdVersion string) {
	state.Isolation = isolation.ModeNone
	state.ActiveMachine = ""
	state.NodeConfig = cfg
	state.NPDVersion = npdVersion
}
