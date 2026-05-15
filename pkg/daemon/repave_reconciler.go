package daemon

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

const DefaultMachineReconcileInterval = 10 * time.Minute

const (
	repaveByAKSMachine = "aks-machine"
	repaveByNode       = "node-change"
)

type repaveReconciler struct {
	log      *slog.Logger
	machines aksmachine.MachineClient
	client   client.Client
	operator NodeOperator
	nodeName string
	// machineEvents queues AKS-machine-driven reconciles so cached Node
	// reads can detect state that existed before the daemon started and then keep
	// polling for ARM-only machine transitions.
	machineEvents            chan event.TypedGenericEvent[struct{}]
	machineReconcileInterval time.Duration
}

type repaveReconcilerOptions struct {
	Log      *slog.Logger
	Machines aksmachine.MachineClient
	Client   client.Client
	Operator NodeOperator
	NodeName string
	// MachineReconcileInterval is the steady-state fallback interval for
	// re-reading the AKS machine resource. Kubernetes Node events wake the
	// controller for node-side signals, but AKS machine changes happen in ARM and
	// do not produce Kubernetes watch events. The fallback catches ARM-only
	// transitions, such as machine deletion after a reset annotation was already
	// observed. Random jitter avoids synchronized ARM reads across large fleets;
	// the tradeoff is that ARM-only transitions can wait up to this duration plus
	// jitter.
	MachineReconcileInterval time.Duration
}

func newRepaveReconciler(opts repaveReconcilerOptions) (*repaveReconciler, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Machines == nil {
		return nil, fmt.Errorf("machine client is nil")
	}
	if opts.Client == nil {
		return nil, fmt.Errorf("kubernetes client is nil")
	}
	if opts.Operator == nil {
		return nil, fmt.Errorf("node operator is nil")
	}
	if opts.NodeName == "" {
		return nil, fmt.Errorf("node name is empty")
	}
	if opts.MachineReconcileInterval == 0 {
		opts.MachineReconcileInterval = DefaultMachineReconcileInterval
	}
	return &repaveReconciler{
		log:                      opts.Log,
		machines:                 opts.Machines,
		client:                   opts.Client,
		operator:                 opts.Operator,
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
		// readiness and reset annotations, deletes unblock applying the next goal,
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
	state, err := r.operator.LoadState(ctx)
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
	case DecisionNoop, DecisionWaitForMachineDelete, DecisionWaitForNodeSignal:
		return nil
	case DecisionReportSucceeded:
		return r.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, decision.Goal.SettingsVersion, decision.Reason)
	case DecisionApplyGoalState:
		return r.applyGoalState(ctx, state, decision.Goal)
	case DecisionResetDelete:
		return r.resetDelete(ctx)
	default:
		return fmt.Errorf("unsupported daemon decision %q", decision.Kind)
	}
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
	active, err := r.operator.FindActiveMachine(ctx, r.log, state)
	if err != nil {
		_ = r.patchStatus(ctx, aksmachine.ProvisioningStateFailed, stateObservedVersion(state), err.Error())
		return err
	}
	newState, err := r.operator.ApplyGoalState(ctx, r.log, active, goal)
	if err != nil {
		_ = r.patchStatus(ctx, aksmachine.ProvisioningStateFailed, stateObservedVersion(state), err.Error())
		return err
	}
	return r.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, newState.AppliedSettingsVersion, "machine goal state applied")
}

func (r *repaveReconciler) resetDelete(ctx context.Context) error {
	// Stage 1 clears local runtime/settings while keeping this daemon alive.
	if err := r.operator.ResetNodeRuntime(ctx, r.log); err != nil {
		return err
	}
	if err := r.operator.ClearState(ctx); err != nil {
		return err
	}

	// Stage 2 publishes lifecycle completion to AKS RP, then stops this daemon.
	if err := r.client.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: r.nodeName}}); apierrors.IsNotFound(err) {
		return r.operator.StopDaemon(ctx, r.log)
	} else if err != nil {
		return fmt.Errorf("delete node %s: %w", r.nodeName, err)
	}
	return r.operator.StopDaemon(ctx, r.log)
}

func (r *repaveReconciler) patchStatus(ctx context.Context, provisioningState aksmachine.ProvisioningState, observedSettingsVersion string, message string) error {
	return r.machines.PatchStatus(ctx, aksmachine.Status{
		ProvisioningState:       provisioningState,
		ObservedSettingsVersion: observedSettingsVersion,
		Message:                 message,
	})
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
