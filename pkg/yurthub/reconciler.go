package yurthub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// The journal shares the worker's lifetime and is never inherited by a repave.
const hubWorkerJournal = "var/lib/aks-flex-node/hub-transition.json"

// Reconciler runs the active worker's transition engine on every Machine
// read; RP changes DT intent without changing the Machine ETag. After a failure it
// waits (1 minute, doubling to 15) so Node events don't repeat 2-minute readiness
// waits on the shared queue; a changed intent or worker runs immediately. Only
// the serialized daemon queue calls it, so retry state needs no lock.
type Reconciler struct {
	log        *slog.Logger
	operations func(machineDir string) (hubTransitionOperations, error)
	now        func() time.Time

	setCondition func(context.Context, corev1.NodeCondition) error
	last         *corev1.NodeCondition // last result, reported again while waiting to retry
	// heartbeat refreshes an unchanged condition, except Disabled, so readers can
	// tell it's current.
	heartbeat time.Duration

	retryAfter   time.Time
	retryDelay   time.Duration
	retryKey     string // intent and worker the delay applies to
	retryMessage string
}

const (
	hubRetryMin = time.Minute
	hubRetryMax = 15 * time.Minute
	// hubHeartbeatDefault matches the default Machine poll interval.
	hubHeartbeatDefault = 10 * time.Minute
)

// NewReconciler builds a DT reconciler using the node client. Calls must be
// serialized with other worker lifecycle operations.
func NewReconciler(log *slog.Logger, nodeName, nodePool, serverURL string, nodes typedcorev1.NodeInterface, heartbeat time.Duration) *Reconciler {
	if heartbeat <= 0 {
		heartbeat = hubHeartbeatDefault
	}
	return &Reconciler{
		log: log,
		operations: func(machineDir string) (hubTransitionOperations, error) {
			return NewOperations(Config{
				Log:        log,
				MachineDir: machineDir,
				NodeName:   nodeName,
				NodePool:   nodePool,
				ServerURL:  serverURL,
				Nodes:      nodes,
			})
		},
		now:          time.Now,
		setCondition: nodeConditionWriter(nodes, nodeName),
		heartbeat:    heartbeat,
	}
}

// Reconcile applies authoritative intent to the current worker. Machine is its
// stable identifier and root its host rootfs path; both are empty without a
// worker. Node is the live snapshot. Nil intent leaves existing state untouched.
func (a *Reconciler) Reconcile(ctx context.Context, intent *bool, machine, root string, node *corev1.Node) error {
	// Nil means this snapshot carried no authoritative value.
	if intent == nil {
		return nil
	}

	key := fmt.Sprintf("%s/%t", machine, *intent)
	if key == a.retryKey && a.now().Before(a.retryAfter) {
		if a.last != nil {
			// Keep the heartbeat fresh and retry a failed write while waiting.
			a.reportCondition(ctx, currentHubCondition(node), *a.last)
		}
		a.log.Debug("waiting before retrying hub", "machine", machine, "retryAfter", a.retryAfter, "lastError", a.retryMessage)
		return nil
	}

	if machine == "" {
		if !*intent {
			return nil // nothing to disable without an active worker
		}
		err := fmt.Errorf("hub reconcile requires a valid active machine")
		a.backoff(key, 0, err)
		return err
	}

	err := a.reconcile(ctx, machine, root, intent)
	result := hubCondition(*intent, err)
	a.last = &result
	a.reportCondition(ctx, currentHubCondition(node), result)
	switch {
	case err == nil:
		a.retryKey, a.retryDelay = "", 0
		return nil
	case errors.Is(err, ErrUnsupportedWorker):
		// Not a failure: nothing in the worker changed and kubelet stays on the
		// direct API. The journal keeps the request, so a later agent that
		// supports the worker enables Hub, and false still converges.
		a.log.Warn("disconnect tolerance requested but not enabled on this node", "machine", machine, "reason", err.Error())
		a.backoff(key, hubRetryMax, err)
		return nil
	default:
		a.backoff(key, 0, err)
		return err
	}
}

// backoff delays the next attempt for key. A fixed delay overrides doubling.
func (a *Reconciler) backoff(key string, fixed time.Duration, err error) {
	if key != a.retryKey {
		a.retryDelay = 0
	}
	switch {
	case fixed > 0:
		a.retryDelay = fixed
	case a.retryDelay == 0:
		a.retryDelay = hubRetryMin
	default:
		a.retryDelay = min(2*a.retryDelay, hubRetryMax)
	}
	a.retryKey, a.retryMessage = key, err.Error()
	a.retryAfter = a.now().Add(a.retryDelay)
}

func (a *Reconciler) reconcile(ctx context.Context, machine, root string, intent *bool) error {
	store := fileHubTransitionStore{root: root, path: hubWorkerJournal}
	// A worker that never started a transition has nothing to disable. Skip
	// building operations, which validate settings only enabling needs.
	if !*intent {
		if state, err := store.Load(ctx); err == nil && state == nil {
			return nil
		}
	}

	ops, err := a.operations(root)
	if err != nil {
		return fmt.Errorf("create hub operations: %w", err)
	}
	if ops == nil {
		return fmt.Errorf("hub operations are nil")
	}

	transition, err := newHubTransition(store, ops)
	if err != nil {
		return err
	}
	return transition.Reconcile(ctx, machine, intent)
}
