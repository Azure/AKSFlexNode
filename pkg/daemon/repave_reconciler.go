package daemon

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const DefaultMachineReconcileInterval = 10 * time.Minute

const (
	repaveByAKSMachine = "aks-machine"
	repaveByNode       = "node-change"
)

const (
	DeletionTaintKey    = "kubernetes.azure.com/flex-node-deleting"
	DeletionTaintValue  = "true"
	DeletionTaintEffect = corev1.TaintEffectNoSchedule
)

type decisionKind string

const (
	decisionNoop                 decisionKind = "Noop"
	decisionApplyGoalState       decisionKind = "ApplyGoalState"
	decisionResetDelete          decisionKind = "ResetDelete"
	decisionWaitForMachineDelete decisionKind = "WaitForMachineDelete"
	decisionWaitForNodeSignal    decisionKind = "WaitForNodeSignal"
	decisionReportSucceeded      decisionKind = "ReportSucceeded"
)

type machineSnapshot struct {
	machine  *aksmachine.Machine
	notFound bool
}

type nodeSnapshot struct {
	node *corev1.Node
}

type decision struct {
	Kind   decisionKind
	Goal   aksmachine.GoalState
	Reason string
}

type repaveReconciler struct {
	log         *slog.Logger
	machines    aksmachine.MachineClient
	client      client.Client
	runtime     agentruntime.Runtime
	state       stateStore
	cfg         *config.Config
	refresher   bootstrapDataRefresher
	resolve     machineGoalResolver
	hookUnits   func(*slog.Logger) phases.Task
	uninstall   func(*slog.Logger) phases.Task
	clearConfig phases.Task
	nodeName    string
	hub         hubMachineReconciler
	// machineEvents queues AKS-machine-driven reconciles so cached Node
	// reads can detect state that existed before the daemon started and then keep
	// polling for ARM-only machine transitions.
	machineEvents            chan event.TypedGenericEvent[struct{}]
	machineReconcileInterval time.Duration
}

type repaveReconcilerOptions struct {
	Log         *slog.Logger
	Machines    aksmachine.MachineClient
	Client      client.Client
	Runtime     agentruntime.Runtime
	State       stateStore
	Config      *config.Config
	Refresher   bootstrapDataRefresher
	HookUnits   func(*slog.Logger) phases.Task
	Uninstall   func(*slog.Logger) phases.Task
	ClearConfig phases.Task
	NodeName    string
	// Hub reconciles disconnect-tolerance intent from Machine reads. Optional so
	// tests and callers without Hub support can omit it.
	Hub hubMachineReconciler
	// MachineReconcileInterval is the steady-state fallback interval for
	// re-reading the AKS machine resource. Kubernetes Node events wake the
	// controller for node-side signals, but AKS machine changes happen in ARM and
	// do not produce Kubernetes watch events. The fallback catches ARM-only
	// transitions, such as machine deletion after a deletion taint was already
	// observed. Random jitter avoids synchronized ARM reads across large fleets;
	// the tradeoff is that ARM-only transitions can wait up to this duration plus
	// jitter.
	MachineReconcileInterval time.Duration
}

func newRepaveReconciler(opts repaveReconcilerOptions) (*repaveReconciler, error) {
	if opts.Machines == nil {
		return nil, fmt.Errorf("machine client is nil")
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("kubernetes client is nil")
	}
	if opts.Runtime == nil {
		return nil, fmt.Errorf("worker runtime is nil")
	}
	if opts.NodeName == "" {
		return nil, fmt.Errorf("node name is empty")
	}
	if opts.State == nil {
		return nil, fmt.Errorf("state store is nil")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if opts.Refresher == nil {
		return nil, fmt.Errorf("bootstrap data refresher is nil")
	}
	if opts.HookUnits == nil {
		return nil, fmt.Errorf("nspawn hook unit task is nil")
	}
	if opts.Uninstall == nil {
		return nil, fmt.Errorf("uninstall task factory is nil")
	}
	if opts.ClearConfig == nil {
		return nil, fmt.Errorf("config cleanup task is nil")
	}
	if opts.MachineReconcileInterval == 0 {
		opts.MachineReconcileInterval = DefaultMachineReconcileInterval
	}
	return &repaveReconciler{
		log:                      opts.Log,
		machines:                 opts.Machines,
		client:                   opts.Client,
		runtime:                  opts.Runtime,
		state:                    opts.State,
		cfg:                      opts.Config,
		refresher:                opts.Refresher,
		resolve:                  config.ResolveMachineGoalState,
		hookUnits:                opts.HookUnits,
		uninstall:                opts.Uninstall,
		clearConfig:              opts.ClearConfig,
		hub:                      opts.Hub,
		nodeName:                 opts.NodeName,
		machineEvents:            make(chan event.TypedGenericEvent[struct{}], 1),
		machineReconcileInterval: opts.MachineReconcileInterval,
	}, nil
}

func (r *repaveReconciler) SetupController(b *builder.TypedBuilder[daemon.Request]) *builder.TypedBuilder[daemon.Request] {
	// Seed the first reconcile because Kubernetes watches only report future Node
	// events; this also starts the fallback AKS-machine poll loop.
	r.machineEvents <- event.TypedGenericEvent[struct{}]{}

	return b.Watches(
		&corev1.Node{},
		handler.TypedEnqueueRequestsFromMapFunc(r.mapNode),
		// Every local Node event can carry a daemon signal: creates/updates expose
		// readiness and deletion taints, deletes unblock applying the next goal,
		// and generic events preserve controller-runtime cache resync behavior.
		builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return e.Object.GetName() == r.nodeName },
			UpdateFunc:  func(e event.UpdateEvent) bool { return e.ObjectNew.GetName() == r.nodeName },
			DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetName() == r.nodeName },
			GenericFunc: func(e event.GenericEvent) bool { return e.Object.GetName() == r.nodeName },
		}),
	).WatchesRawSource(source.TypedChannel(
		r.machineEvents,
		handler.TypedEnqueueRequestsFromMapFunc(r.mapMachineEvent),
	))
}

func (r *repaveReconciler) ReconcileRepave(ctx context.Context, source string) (reconcile.Result, error) {
	if err := r.reconcileOnce(ctx); err != nil {
		return reconcile.Result{}, err
	}
	if source == repaveByAKSMachine {
		return reconcile.Result{RequeueAfter: r.machineReconcileInterval + machineReconcileJitter(r.machineReconcileInterval)}, nil
	}
	return reconcile.Result{}, nil
}

func (r *repaveReconciler) reconcileOnce(ctx context.Context) error {
	state, err := r.state.Load(ctx)
	if err != nil {
		_ = r.patchStatus(ctx, aksmachine.ProvisioningStateFailed, "", err.Error())
		return err
	}

	machineSnap, err := r.machineSnapshot(ctx)
	if err != nil {
		return err
	}
	nodeSnap, err := r.nodeSnapshot(ctx)
	if err != nil {
		return err
	}

	decision := decide(machineSnap, nodeSnap, state)
	r.log.Info("daemon reconcile decision", "decision", decision.Kind, "reason", decision.Reason)

	switch decision.Kind {
	case decisionNoop, decisionWaitForMachineDelete:
		return nil
	case decisionWaitForNodeSignal:
		r.reconcileHub(ctx, machineSnap, nodeSnap, state)
		return nil
	case decisionReportSucceeded:
		// Report first so Hub work (which can download and wait for readiness)
		// never delays Machine status.
		err := r.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, decision.Goal.SettingsVersion, decision.Reason)
		r.reconcileHub(ctx, machineSnap, nodeSnap, state)
		return err
	case decisionApplyGoalState:
		return r.applyGoalState(ctx, state, decision.Goal)
	case decisionResetDelete:
		return r.resetDelete(ctx)
	default:
		return fmt.Errorf("unsupported daemon decision %q", decision.Kind)
	}
}

// reconcileHub applies DT intent in place while the Node exists, including while
// a newer Machine goal waits for its repave signal. Failures are logged, not
// returned, so they don't switch the Machine poll to fast error backoff; the
// reconciler paces its own retries and the journal keeps progress.
func (r *repaveReconciler) reconcileHub(ctx context.Context, machineSnap machineSnapshot, nodeSnap nodeSnapshot, state *State) {
	if r.hub == nil || nodeSnap.node == nil || machineSnap.machine == nil {
		return
	}
	var worker yurthub.Worker
	if state != nil && validActiveMachine(state.ActiveMachine) {
		view, err := r.runtime.Worker(r.log, agentruntime.Node{Slot: state.ActiveMachine, Config: state.NodeConfig})
		if err != nil {
			r.log.Error("resolve worker for hub reconcile", "error", err)
			return
		}
		worker = hubWorker(state.ActiveMachine, view)
	}
	if err := r.hub.Reconcile(ctx, machineSnap.machine.Goal.DisconnectToleranceEnabled, worker, nodeSnap.node); err != nil {
		r.log.Error("hub reconcile failed; will retry on a later read", "error", err)
	}
}

// hubMachineReconciler runs on the same serialized queue as repave and MachineOperations.
type hubMachineReconciler interface {
	Reconcile(context.Context, *bool, yurthub.Worker, *corev1.Node) error
}

func (r *repaveReconciler) machineSnapshot(ctx context.Context) (machineSnapshot, error) {
	machine, err := r.machines.Get(ctx)
	var notFound *aksmachine.NotFoundError
	if errors.As(err, &notFound) {
		return machineSnapshot{notFound: true}, nil
	}
	if err != nil {
		return machineSnapshot{}, err
	}
	if err := machine.Validate(); err != nil {
		return machineSnapshot{}, fmt.Errorf("validate AKS machine snapshot: %w", err)
	}
	return machineSnapshot{machine: machine}, nil
}

func (r *repaveReconciler) nodeSnapshot(ctx context.Context) (nodeSnapshot, error) {
	var node corev1.Node
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node); apierrors.IsNotFound(err) {
		return nodeSnapshot{}, nil
	} else if err != nil {
		return nodeSnapshot{}, fmt.Errorf("get node %s: %w", r.nodeName, err)
	}
	return nodeSnapshot{node: &node}, nil
}

func (r *repaveReconciler) applyGoalState(ctx context.Context, state *State, goal aksmachine.GoalState) error {
	if err := r.patchStatus(ctx, aksmachine.ProvisioningStateReconciling, stateObservedVersion(state), "applying machine goal state"); err != nil {
		return err
	}
	newState, err := r.executeGoalState(ctx, goal)
	if err != nil {
		_ = r.patchStatus(ctx, aksmachine.ProvisioningStateFailed, stateObservedVersion(state), err.Error())
		return err
	}
	return r.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, newState.AppliedSettingsVersion, "machine goal state applied")
}

func (r *repaveReconciler) executeGoalState(ctx context.Context, goal aksmachine.GoalState) (*State, error) {
	active, err := activeMachineFromStore(ctx, r.state)
	if err != nil {
		return nil, err
	}
	if active.State.Isolation != r.cfg.Bootstrap.Isolation {
		return nil, fmt.Errorf("worker isolation change requires reset")
	}
	cfg, err := configForGoalState(ctx, r.log, r.cfg, r.refresher, goal)
	if err != nil {
		return nil, err
	}
	newMachine := goalstates.AlternateMachine(active.Name)
	if cfg.Bootstrap.Isolation == isolation.ModeNone {
		newMachine = active.Name
	}
	r.log.Info("starting worker goal-state apply",
		"oldMachine", active.Name,
		"newMachine", newMachine,
		"settingsVersion", goal.SettingsVersion,
		"kubernetesVersion", cfg.Components.Kubernetes,
	)
	resolved, err := r.resolve(ctx, r.log, cfg, newMachine)
	if err != nil {
		return nil, fmt.Errorf("resolve goal state for repave: %w", err)
	}
	previous := agentruntime.Node{Slot: active.Name, Config: active.State.NodeConfig}
	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:      newMachine,
			Config:    resolved.AgentConfig,
			GoalState: resolved.GoalState,
		},
		Previous: &previous,
		Archives: resolved.ContainerImageArchives,
	}
	next := nextAppliedState(active.State, goal, &activeMachine{Name: newMachine})
	next.Isolation = cfg.Bootstrap.Isolation
	if next.Isolation == isolation.ModeNone || next.Isolation == isolation.ModeContainer {
		if resolved.AgentConfig == nil || previous.Config == nil {
			return nil, fmt.Errorf("%s repave requires applied and target runtime configurations", next.Isolation)
		}
		next.NodeConfig = resolved.AgentConfig.DeepCopy()
	}

	if err := r.runtime.PrepareUpgrade(r.log, input).Do(ctx); err != nil {
		return nil, fmt.Errorf("prepare worker upgrade: %w", err)
	}
	var tasks []phases.Task
	switch cfg.Bootstrap.Isolation {
	case isolation.ModeNone:
		view, err := r.runtime.Worker(r.log, previous)
		if err != nil {
			return nil, fmt.Errorf("resolve worker for hub cleanup: %w", err)
		}
		if err := yurthub.ResetHost(ctx, hubWorker(active.Name, view)); err != nil {
			return nil, fmt.Errorf("clean host hub before repave: %w", err)
		}
	case isolation.ModeNSpawn:
		tasks = append(tasks, r.hookUnits(r.log))
	case isolation.ModeContainer:
	default:
		return nil, fmt.Errorf("unsupported worker isolation %q", cfg.Bootstrap.Isolation)
	}
	tasks = append(tasks,
		r.runtime.Start(r.log, input),
		&saveStateTask{store: r.state, state: next},
		r.runtime.Retire(r.log, input),
	)
	if err := phases.Serial(r.log, tasks...).Do(ctx); err != nil {
		return nil, fmt.Errorf("apply machine goal state: %w", err)
	}
	return next, nil
}

func (r *repaveReconciler) resetDelete(ctx context.Context) error {
	// Stage 1 clears local runtime/settings while keeping this daemon alive.
	if err := phases.ExecuteTask(ctx, r.log, ResetWorker(r.log, r.runtime, ResetHostHub(r.log))); err != nil {
		return err
	}
	if err := phases.ExecuteTask(ctx, r.log, r.clearConfig); err != nil {
		return fmt.Errorf("%s: %w", r.clearConfig.Name(), err)
	}

	// Stage 2 stops this daemon. The RP owns Kubernetes Node deletion after it
	// observes the VM-side cleanup signal through daemon shutdown/Node readiness.
	return phases.ExecuteTask(ctx, r.log, r.uninstall(r.log))
}

func (r *repaveReconciler) patchStatus(ctx context.Context, provisioningState aksmachine.ProvisioningState, observedSettingsVersion string, message string) error {
	return r.machines.PatchStatus(ctx, aksmachine.Status{
		ProvisioningState:       provisioningState,
		ObservedSettingsVersion: observedSettingsVersion,
		Message:                 message,
	})
}

func decide(machine machineSnapshot, node nodeSnapshot, state *State) decision {
	nodeExists := node.node != nil
	deleteRequested := nodeExists && hasDeletionSignal(node.node.Spec.Taints)
	if deleteRequested {
		if machine.notFound {
			return decision{Kind: decisionResetDelete, Reason: "deletion signal present and machine resource is gone"}
		}
		return decision{Kind: decisionWaitForMachineDelete, Reason: "deletion signal present but machine resource still exists"}
	}

	if machine.notFound {
		return decision{Kind: decisionWaitForNodeSignal, Reason: "machine resource is gone without deletion signal"}
	}
	if machine.machine == nil {
		return decision{Kind: decisionNoop, Reason: "machine snapshot is empty"}
	}

	goal := machine.machine.Goal
	if node.node == nil {
		if goalApplied(goal, state) {
			return decision{Kind: decisionReportSucceeded, Goal: goal, Reason: "node is absent but goal state is already applied"}
		}
		return decision{Kind: decisionApplyGoalState, Goal: goal, Reason: "node deletion observed and goal state is not applied"}
	}

	if goalApplied(goal, state) {
		return decision{Kind: decisionReportSucceeded, Goal: goal, Reason: "goal state is applied"}
	}

	return decision{Kind: decisionWaitForNodeSignal, Goal: goal, Reason: "goal state differs but node deletion trigger is absent"}
}

func goalApplied(goal aksmachine.GoalState, state *State) bool {
	if state == nil {
		return false
	}
	return goal.SettingsVersion != "" && state.AppliedSettingsVersion == goal.SettingsVersion
}

func hasDeletionSignal(taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Key == DeletionTaintKey && taint.Effect == DeletionTaintEffect && strings.EqualFold(strings.TrimSpace(taint.Value), DeletionTaintValue) {
			return true
		}
	}
	return false
}

func stateObservedVersion(state *State) string {
	if state == nil {
		return ""
	}
	return state.AppliedSettingsVersion
}

func machineReconcileJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	maxJitter := interval / 10
	if maxJitter <= 0 {
		return 0
	}
	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxJitter)+1))
	if err != nil {
		return 0
	}
	return time.Duration(jitter.Int64())
}

func (r *repaveReconciler) mapMachineEvent(context.Context, struct{}) []daemon.Request {
	return []daemon.Request{daemon.NewRepaveRequest(repaveByAKSMachine)}
}

func (r *repaveReconciler) mapNode(_ context.Context, obj client.Object) []daemon.Request {
	if obj.GetName() != r.nodeName {
		return nil
	}
	return []daemon.Request{daemon.NewRepaveRequest(repaveByNode)}
}

var _ daemon.RepaveReconciler = (*repaveReconciler)(nil)
