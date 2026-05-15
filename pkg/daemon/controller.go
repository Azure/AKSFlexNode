package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const DefaultMachineReconcileInterval = 10 * time.Minute

type daemonRunner struct {
	log                      *slog.Logger
	machines                 aksmachine.MachineClient
	client                   client.Client
	operator                 NodeOperator
	nodeName                 string
	machineReconcileInterval time.Duration
}

type runnerOptions struct {
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

func newDaemonRunner(opts runnerOptions) (*daemonRunner, error) {
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
	return &daemonRunner{
		log:                      opts.Log,
		machines:                 opts.Machines,
		client:                   opts.Client,
		operator:                 opts.Operator,
		nodeName:                 opts.NodeName,
		machineReconcileInterval: opts.MachineReconcileInterval,
	}, nil
}

func (r *daemonRunner) run(ctx context.Context, restCfg *rest.Config) error {
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", r.nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}
	repaves := newRepaveReconciler(r)
	if err := agentdaemon.SetupController("aks-flex-node-daemon", mgr, noopMachineOperationReconciler{}, repaves); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}
	r.client = mgr.GetClient()
	repaves.enqueueInitialReconcile(ctx)

	err = mgr.Start(ctx)
	r.log.Info("daemon shutting down")
	return err
}

func (r *daemonRunner) reconcileOnce(ctx context.Context) error {
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

func (r *daemonRunner) machineSnapshot(ctx context.Context) (machineSnapshot, error) {
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

func (r *daemonRunner) nodeSnapshot(ctx context.Context) (nodeSnapshot, error) {
	var node corev1.Node
	if err := r.client.Get(ctx, client.ObjectKey{Name: r.nodeName}, &node); apierrors.IsNotFound(err) {
		return nodeSnapshot{}, nil
	} else if err != nil {
		return nodeSnapshot{}, fmt.Errorf("get node %s: %w", r.nodeName, err)
	}
	return nodeSnapshot{node: &node}, nil
}

func (r *daemonRunner) applyGoalState(ctx context.Context, state *State, goal aksmachine.GoalState) error {
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

func (r *daemonRunner) resetDelete(ctx context.Context) error {
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

func (r *daemonRunner) patchStatus(ctx context.Context, provisioningState aksmachine.ProvisioningState, observedSettingsVersion string, message string) error {
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

type noopMachineOperationReconciler struct{}

func (noopMachineOperationReconciler) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b
}

func (noopMachineOperationReconciler) ReconcileMachineOperation(context.Context, string) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

var _ agentdaemon.MachineOperationRequestReconciler = noopMachineOperationReconciler{}
