package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
)

const (
	// nspawnMachinesDir mirrors the Unbounded goal-state machine root.
	nspawnMachinesDir = "/var/lib/machines"
	// hubWorkerJournal is relative to the worker rootfs, so the journal shares the
	// worker generation's lifetime and is never inherited by a repaved worker.
	hubWorkerJournal = "var/lib/aks-flex-node/hub-transition.json"
)

// hubMachineAdapter runs the active worker's transition engine on every Machine
// read; RP changes DT intent without changing the Machine ETag. After a failure it
// waits (1 minute, doubling to 15) so Node events don't repeat 2-minute readiness
// waits on the shared queue; a changed intent or worker runs immediately. Only
// the serialized daemon queue calls it, so retry state needs no lock.
type hubMachineAdapter struct {
	log         *slog.Logger
	machinesDir string
	operations  func(machineDir string) (hubTransitionOperations, error)
	now         func() time.Time

	// setCondition writes the Node's DT condition; nil disables reporting.
	setCondition func(context.Context, corev1.NodeCondition) error
	last         *corev1.NodeCondition // last result, reported again while waiting to retry
	// heartbeat refreshes an unchanged condition so readers can tell it's current.
	heartbeat time.Duration

	retryAfter   time.Time
	retryDelay   time.Duration
	retryKey     string // intent and worker the delay applies to
	retryMessage string
}

// hubConditionType reports DT state on the Node, next to kubelet's and NPD's
// conditions, so a customer can see why Hub isn't active without agent logs.
const hubConditionType corev1.NodeConditionType = "DisconnectTolerance"

const (
	hubRetryMin = time.Minute
	hubRetryMax = 15 * time.Minute
	// hubHeartbeatDefault matches the default Machine poll interval.
	hubHeartbeatDefault = 10 * time.Minute
)

func newHubMachineAdapter(operations func(machineDir string) (hubTransitionOperations, error)) (*hubMachineAdapter, error) {
	if operations == nil {
		return nil, fmt.Errorf("hub operations factory is nil")
	}
	return &hubMachineAdapter{log: slog.Default(), machinesDir: nspawnMachinesDir, operations: operations, now: time.Now, heartbeat: hubHeartbeatDefault}, nil
}

// newDaemonHubAdapter builds production Hub operations and the Node condition
// writer, both using the daemon's own node credential (restCfg).
func newDaemonHubAdapter(cfg *config.Config, log *slog.Logger, restCfg *rest.Config) (*hubMachineAdapter, error) {
	// Per-request timeout so a stalled Node call can't hold the daemon queue.
	nodeCfg := rest.CopyConfig(restCfg)
	nodeCfg.Timeout = 30 * time.Second
	clientset, err := kubernetes.NewForConfig(nodeCfg)
	if err != nil {
		return nil, fmt.Errorf("create hub node client: %w", err)
	}
	adapter, err := newHubMachineAdapter(func(machineDir string) (hubTransitionOperations, error) {
		return yurthub.NewOperations(yurthub.Config{
			Log:        log,
			MachineDir: machineDir,
			NodeName:   cfg.Agent.NodeName,
			NodePool:   cfg.Azure.TargetAgentPoolName,
			ServerURL:  cfg.APIServerURL(),
			Nodes:      clientset.CoreV1().Nodes(),
		})
	})
	if err != nil {
		return nil, err
	}
	adapter.log = log
	adapter.setCondition = nodeConditionWriter(clientset.CoreV1().Nodes(), cfg.Agent.NodeName)
	if interval := time.Duration(cfg.Agent.MachineReconcileInterval); interval > 0 {
		adapter.heartbeat = interval
	}
	return adapter, nil
}

// ReconcileHub applies DT intent to the active worker. node is the current Node
// (nil if unknown); its condition is compared before writing a new one.
func (a *hubMachineAdapter) ReconcileHub(ctx context.Context, goal aksmachine.GoalState, state *State, node *corev1.Node) error {
	// Nil means this snapshot carried no authoritative value (for example an
	// undecodable field). Keep current state; an omitted field was already
	// mapped to false by the Machine client, per the agreed contract.
	intent := goal.DisconnectToleranceEnabled
	if intent == nil {
		return nil
	}
	machine := ""
	if state != nil && validActiveMachine(state.ActiveMachine) {
		machine = state.ActiveMachine
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
	err := a.reconcile(ctx, machine, intent)
	result := hubCondition(*intent, err)
	a.last = &result
	a.reportCondition(ctx, currentHubCondition(node), result)
	switch {
	case err == nil:
		a.retryKey, a.retryDelay = "", 0
		return nil
	case errors.Is(err, yurthub.ErrUnsupportedWorker):
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

// nodeConditionWriter patches one Node condition, merged by type so kubelet's
// and NPD's conditions are kept. The daemon credential is the node identity,
// which may patch its own status.
func nodeConditionWriter(nodes typedcorev1.NodeInterface, name string) func(context.Context, corev1.NodeCondition) error {
	return func(ctx context.Context, condition corev1.NodeCondition) error {
		patch, err := json.Marshal(map[string]any{"status": map[string]any{"conditions": []corev1.NodeCondition{condition}}})
		if err != nil {
			return err
		}
		_, err = nodes.PatchStatus(ctx, name, patch)
		return err
	}
}

// reportCondition writes the condition when it differs from the Node's, or when the
// Node's heartbeat is older than a.heartbeat (or in the future). Comparing against
// the live Node (not memory) repairs external edits and keeps the transition time
// across agent restarts. A failed write is logged, never fails the reconcile, and
// is retried on the next read.
func (a *hubMachineAdapter) reportCondition(ctx context.Context, current *corev1.NodeCondition, condition corev1.NodeCondition) {
	if a.setCondition == nil {
		return
	}
	now := a.now()
	if current != nil && current.Status == condition.Status && current.Reason == condition.Reason && current.Message == condition.Message {
		if age := now.Sub(current.LastHeartbeatTime.Time); age >= 0 && age < a.heartbeat {
			return
		}
	}
	condition.LastHeartbeatTime, condition.LastTransitionTime = metav1.NewTime(now), metav1.NewTime(now)
	if current != nil && current.Status == condition.Status {
		condition.LastTransitionTime = current.LastTransitionTime
	}
	if err := a.setCondition(ctx, condition); err != nil {
		a.log.Warn("could not update the node disconnect-tolerance condition", "error", err)
	}
}

func currentHubCondition(node *corev1.Node) *corev1.NodeCondition {
	if node == nil {
		return nil
	}
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == hubConditionType {
			return &node.Status.Conditions[i]
		}
	}
	return nil
}

// hubConditionHelp leads to troubleshooting steps for each condition reason.
// TODO: replace with the aka.ms link once it exists.
const hubConditionHelp = "aka.ms/TODO"

// hubCondition maps a result to a short, fixed message. Details stay in the
// agent log, so the Node never carries URLs or error text.
func hubCondition(intent bool, err error) corev1.NodeCondition {
	condition := corev1.NodeCondition{Type: hubConditionType}
	see := " See " + hubConditionHelp + "."
	switch {
	case err == nil && intent:
		condition.Status, condition.Reason, condition.Message = corev1.ConditionTrue, "Enabled", "YurtHub is serving kubelet."
	case err == nil:
		condition.Status, condition.Reason, condition.Message = corev1.ConditionFalse, "Disabled", "Disconnect tolerance is not enabled."
	case errors.Is(err, yurthub.ErrUnsupportedWorker):
		condition.Status, condition.Reason, condition.Message = corev1.ConditionFalse, "Unsupported", "Disconnect tolerance is not supported on this node."+see
	case errors.Is(err, yurthub.ErrPackageDownload):
		condition.Status, condition.Reason, condition.Message = corev1.ConditionFalse, "PackageDownloadFailed", "Could not download YurtHub from packages.microsoft.com."+see
	case errors.Is(err, errHubConverge):
		// Hub was enabled and kubelet still routes through it.
		condition.Status, condition.Reason, condition.Message = corev1.ConditionTrue, "Degraded", "YurtHub is enabled but isn't healthy or up to date."+see
	case intent:
		condition.Status, condition.Reason, condition.Message = corev1.ConditionUnknown, "EnableFailed", "Enabling disconnect tolerance failed."+see
	default:
		condition.Status, condition.Reason, condition.Message = corev1.ConditionUnknown, "DisableFailed", "Disabling disconnect tolerance failed."+see
	}
	return condition
}

// backoff delays the next attempt for key. A fixed delay overrides doubling.
func (a *hubMachineAdapter) backoff(key string, fixed time.Duration, err error) {
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

func (a *hubMachineAdapter) reconcile(ctx context.Context, machine string, intent *bool) error {
	machineDir := filepath.Join(a.machinesDir, machine)
	ops, err := a.operations(machineDir)
	if err != nil {
		return fmt.Errorf("create hub operations: %w", err)
	}
	if ops == nil {
		return fmt.Errorf("hub operations are nil")
	}
	transition, err := newHubTransition(fileHubTransitionStore{root: machineDir, path: hubWorkerJournal}, ops)
	if err != nil {
		return err
	}
	return transition.Reconcile(ctx, machine, intent)
}
