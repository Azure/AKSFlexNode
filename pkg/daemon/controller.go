package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

type queueItemKind string

const DefaultMachineReconcileInterval = 10 * time.Minute

const (
	queueItemNodeChange    queueItemKind = "nodeChange"
	queueItemMachineChange queueItemKind = "machineChange"
)

type daemonRequest struct {
	Kind queueItemKind
	Name string
}

type Controller struct {
	log      *slog.Logger
	machines aksmachine.MachineClient
	client   client.Client
	operator NodeOperator
	nodeName string
	// initialEvents queues the first reconcile after manager startup so cached
	// Node reads can detect state that existed before the daemon started.
	initialEvents            chan event.TypedGenericEvent[string]
	machineReconcileInterval time.Duration
}

type ControllerOptions struct {
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

func NewController(opts ControllerOptions) (*Controller, error) {
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
	return &Controller{
		log:                      opts.Log,
		machines:                 opts.Machines,
		client:                   opts.Client,
		operator:                 opts.Operator,
		nodeName:                 opts.NodeName,
		initialEvents:            make(chan event.TypedGenericEvent[string], 1),
		machineReconcileInterval: opts.MachineReconcileInterval,
	}, nil
}

func (c *Controller) Run(ctx context.Context, restCfg *rest.Config) error {
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", c.nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}
	if err := c.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}
	c.client = mgr.GetClient()
	c.enqueueInitialReconcile(ctx)

	err = mgr.Start(ctx)
	c.log.Info("daemon shutting down")
	return err
}

func (c *Controller) SetupWithManager(mgr ctrl.Manager) error {
	return builder.TypedControllerManagedBy[daemonRequest](mgr).
		Named("aks-flex-node-daemon").
		Watches(
			&corev1.Node{},
			handler.TypedEnqueueRequestsFromMapFunc(c.mapNode),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc:  func(e event.CreateEvent) bool { return e.Object.GetName() == c.nodeName },
				UpdateFunc:  func(e event.UpdateEvent) bool { return e.ObjectNew.GetName() == c.nodeName },
				DeleteFunc:  func(e event.DeleteEvent) bool { return e.Object.GetName() == c.nodeName },
				GenericFunc: func(e event.GenericEvent) bool { return e.Object.GetName() == c.nodeName },
			}),
		).
		WatchesRawSource(source.TypedChannel(
			c.initialEvents,
			handler.TypedEnqueueRequestsFromMapFunc(c.mapInitialEvent),
		)).
		WithOptions(controller.TypedOptions[daemonRequest]{MaxConcurrentReconciles: 1}).
		Complete(c)
}

func (c *Controller) Reconcile(ctx context.Context, req daemonRequest) (reconcile.Result, error) {
	switch req.Kind {
	case queueItemNodeChange:
		return reconcile.Result{}, c.ReconcileOnce(ctx)
	case queueItemMachineChange:
		if err := c.ReconcileOnce(ctx); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: c.machineReconcileInterval + machineReconcileJitter(c.machineReconcileInterval)}, nil
	default:
		return reconcile.Result{}, nil
	}
}

func machineReconcileJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	maxJitter := interval / 10
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(maxJitter) + 1))
}

func (c *Controller) mapInitialEvent(_ context.Context, name string) []daemonRequest {
	return []daemonRequest{{Kind: queueItemMachineChange, Name: name}}
}

func (c *Controller) mapNode(_ context.Context, obj client.Object) []daemonRequest {
	if obj.GetName() != c.nodeName {
		return nil
	}
	return []daemonRequest{{Kind: queueItemNodeChange, Name: obj.GetName()}}
}

func (c *Controller) enqueueInitialReconcile(ctx context.Context) {
	select {
	case c.initialEvents <- event.TypedGenericEvent[string]{Object: c.nodeName}:
	case <-ctx.Done():
	}
}

func (c *Controller) ReconcileOnce(ctx context.Context) error {
	state, err := c.operator.LoadState(ctx)
	if err != nil {
		_ = c.patchStatus(ctx, aksmachine.ProvisioningStateFailed, "", err.Error())
		return err
	}

	machineSnap, err := c.machineSnapshot(ctx)
	if err != nil {
		return err
	}
	nodeSnap, err := c.nodeSnapshot(ctx)
	if err != nil {
		return err
	}

	decision := decide(machineSnap, nodeSnap, state)
	c.log.Info("daemon reconcile decision", "decision", decision.Kind, "reason", decision.Reason)

	switch decision.Kind {
	case DecisionNoop, DecisionWaitForMachineDelete, DecisionWaitForNodeSignal:
		return nil
	case DecisionReportSucceeded:
		return c.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, decision.Goal.SettingsVersion, decision.Reason)
	case DecisionApplyGoalState:
		return c.applyGoalState(ctx, state, decision.Goal)
	case DecisionResetDelete:
		return c.resetDelete(ctx)
	default:
		return fmt.Errorf("unsupported daemon decision %q", decision.Kind)
	}
}

func (c *Controller) machineSnapshot(ctx context.Context) (machineSnapshot, error) {
	machine, err := c.machines.Get(ctx)
	var notFound *aksmachine.NotFoundError
	if errors.As(err, &notFound) {
		return machineSnapshot{notFound: true}, nil
	}
	if err != nil {
		return machineSnapshot{}, err
	}
	return machineSnapshot{machine: machine}, nil
}

func (c *Controller) nodeSnapshot(ctx context.Context) (nodeSnapshot, error) {
	var node corev1.Node
	if err := c.client.Get(ctx, client.ObjectKey{Name: c.nodeName}, &node); apierrors.IsNotFound(err) {
		return nodeSnapshot{}, nil
	} else if err != nil {
		return nodeSnapshot{}, fmt.Errorf("get node %s: %w", c.nodeName, err)
	}
	return nodeSnapshot{node: &node}, nil
}

func (c *Controller) applyGoalState(ctx context.Context, state *State, goal aksmachine.GoalState) error {
	if err := c.patchStatus(ctx, aksmachine.ProvisioningStateReconciling, stateObservedVersion(state), "applying machine goal state"); err != nil {
		return err
	}
	active, err := c.operator.FindActiveMachine(ctx, c.log, state)
	if err != nil {
		_ = c.patchStatus(ctx, aksmachine.ProvisioningStateFailed, stateObservedVersion(state), err.Error())
		return err
	}
	newState, err := c.operator.ApplyGoalState(ctx, c.log, active, goal)
	if err != nil {
		_ = c.patchStatus(ctx, aksmachine.ProvisioningStateFailed, stateObservedVersion(state), err.Error())
		return err
	}
	return c.patchStatus(ctx, aksmachine.ProvisioningStateSucceeded, newState.AppliedSettingsVersion, "machine goal state applied")
}

func (c *Controller) resetDelete(ctx context.Context) error {
	// Stage 1 clears local runtime/settings while keeping this daemon alive.
	if err := c.operator.ResetNodeRuntime(ctx, c.log); err != nil {
		return err
	}
	if err := c.operator.ClearState(ctx); err != nil {
		return err
	}

	// Stage 2 publishes lifecycle completion to AKS RP, then stops this daemon.
	if err := c.client.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: c.nodeName}}); apierrors.IsNotFound(err) {
		return c.operator.StopDaemon(ctx, c.log)
	} else if err != nil {
		return fmt.Errorf("delete node %s: %w", c.nodeName, err)
	}
	return c.operator.StopDaemon(ctx, c.log)
}

func (c *Controller) patchStatus(ctx context.Context, provisioningState aksmachine.ProvisioningState, observedSettingsVersion string, message string) error {
	return c.machines.PatchStatus(ctx, aksmachine.Status{
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
