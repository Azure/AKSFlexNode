package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const (
	bootIDPath            = "/proc/sys/kernel/random/boot_id"
	hostSoftRebootMessage = "rebooting host operating system"
	hostSoftRebootRetry   = 10 * time.Second
)

func readBootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot ID: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read boot ID: %s is empty", bootIDPath)
	}
	return id, nil
}

func (h *machineOperationHandlers) reconcileHostSoftReboot(ctx context.Context, _ agentdaemon.MachineOperationStore[int64], op agentdaemon.MachineOperation) (ctrl.Result, error) {
	lock, err := h.agentUpgrade.Acquire()
	if errors.Is(err, agentbinary.ErrActivationInProgress) {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("acquire agent activation lock: %w", err)
	}
	defer func() {
		if err := lock.Close(); err != nil {
			h.log.Warn("failed to release agent activation lock", "error", err)
		}
	}()

	blocker, err := unfinishedOutOfBandHostOperation(ctx, h.reader, h.machineName, h.nodeName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if blocker != "" {
		message := fmt.Sprintf("waiting for out-of-band host operation %s", blocker)
		return ctrl.Result{RequeueAfter: hostSoftRebootRetry}, markHostSoftRebootWaiting(ctx, h.client, h.reader, op.Name, message)
	}

	bootID, err := h.readBootID()
	if err != nil {
		return ctrl.Result{}, err
	}
	started, err := startHostSoftReboot(ctx, h.client, h.reader, op.Name, h.machineName, h.nodeName, bootID)
	if err != nil || !started {
		return ctrl.Result{}, err
	}

	// The next daemon judges this from the recorded boot ID, even if shutdown
	// interrupts the command. No other queue item may start in the meantime.
	h.gate.stopping.Store(true)
	h.log.Info("rebooting host for HostSoftReboot", "operation", op.Name, "bootID", bootID)
	if err := h.rebootHost(ctx, h.log); err != nil {
		if ctx.Err() != nil {
			return ctrl.Result{}, nil
		}
		result := agentdaemon.MachineOperationResult[int64]{
			Phase:   machinav1alpha3.OperationPhaseFailed,
			Reason:  "Failed",
			Message: err.Error(),
		}
		h.gate.stopping.Store(false)
		return ctrl.Result{}, finishHostSoftReboot(ctx, h.client, h.reader, op.Name, h.machineName, bootID, result)
	}
	return ctrl.Result{}, nil
}

func startHostSoftReboot(ctx context.Context, c client.Client, reader client.Reader, name, machineName, nodeName, bootID string) (bool, error) {
	started := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op machinav1alpha3.MachineOperation
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}
		if op.Spec.OperationKind != machinav1alpha3.OperationHostSoftReboot || !operationPending(op.Status.Phase) {
			return nil
		}
		if target := operationTargetStatus(&op.Status, machineName); target != nil && !operationPending(target.Phase) {
			return nil
		}
		matches, err := matchesFlexMachine(ctx, reader, &op, machineName, nodeName)
		if err != nil || !matches {
			return err
		}

		now := metav1.Now()
		op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
		op.Status.Message = hostSoftRebootMessage
		if op.Status.StartedAt == nil {
			op.Status.StartedAt = &now
		}
		apimeta.SetStatusCondition(&op.Status.Conditions, metav1.Condition{
			Type:               "Completed",
			Status:             metav1.ConditionFalse,
			Reason:             "InProgress",
			Message:            hostSoftRebootMessage,
			ObservedGeneration: op.Generation,
		})
		target := operationTargetStatus(&op.Status, machineName)
		if target == nil {
			op.Status.Targets = append(op.Status.Targets, machinav1alpha3.MachineOperationTargetStatus{MachineRef: machineName})
			target = &op.Status.Targets[len(op.Status.Targets)-1]
		}
		target.Phase = machinav1alpha3.OperationPhaseInProgress
		target.Message = hostSoftRebootMessage
		target.StartedAt = &now
		target.CompletedAt = nil
		target.BootID = bootID
		if err := c.Status().Update(ctx, &op); err != nil {
			return err
		}

		started = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("record HostSoftReboot boot ID: %w", err)
	}

	return started, nil
}

func finishHostSoftReboot(ctx context.Context, c client.Client, reader client.Reader, name, machineName, bootID string, result agentdaemon.MachineOperationResult[int64]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op machinav1alpha3.MachineOperation
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}
		if op.Spec.OperationKind != machinav1alpha3.OperationHostSoftReboot || op.Status.IsTerminal() {
			return nil
		}
		target := operationTargetStatus(&op.Status, machineName)
		if target == nil || target.Phase != machinav1alpha3.OperationPhaseInProgress || target.BootID != bootID {
			return nil
		}
		now := metav1.Now()
		agentdaemon.SetMachineOperationResult(&op, result, now)
		target.Phase = result.Phase
		target.Message = result.Message
		target.CompletedAt = &now
		return c.Status().Update(ctx, &op)
	})
}

func operationPending(phase machinav1alpha3.OperationPhase) bool {
	return phase == "" || phase == machinav1alpha3.OperationPhasePending
}

func operationTargetStatus(status *machinav1alpha3.MachineOperationStatus, machineName string) *machinav1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == machineName {
			return &status.Targets[i]
		}
	}
	return nil
}

func matchesFlexMachine(ctx context.Context, reader client.Reader, op *machinav1alpha3.MachineOperation, machineName, nodeName string) (bool, error) {
	if op.Spec.MachineRef != "" {
		return op.Spec.MachineRef == machineName, nil
	}
	if op.Spec.MachineSelector == nil {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(op.Spec.MachineSelector)
	if err != nil {
		// Controllers do not execute operations with an invalid selector.
		return false, nil
	}
	var node corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("get Node %s for selector match: %w", nodeName, err)
	}
	return selector.Matches(labels.Set(node.Labels)), nil
}

func unfinishedOutOfBandHostOperation(ctx context.Context, reader client.Reader, machineName, nodeName string) (string, error) {
	var operations machinav1alpha3.MachineOperationList
	if err := reader.List(ctx, &operations); err != nil {
		return "", fmt.Errorf("list MachineOperations: %w", err)
	}
	var oldest *machinav1alpha3.MachineOperation
	for i := range operations.Items {
		op := &operations.Items[i]
		kind := op.Spec.OperationKind
		if !strings.HasPrefix(string(kind), "Host") || kind == machinav1alpha3.OperationHostSoftReboot || kind.IsAllocationOperation() {
			continue
		}
		target := operationTargetStatus(&op.Status, machineName)
		unfinished := target != nil && target.HasOutstandingBootMedia()
		if !unfinished && !op.Status.IsTerminal() {
			if len(op.Status.Targets) > 0 {
				unfinished = target != nil && target.Phase != machinav1alpha3.OperationPhaseComplete && target.Phase != machinav1alpha3.OperationPhaseFailed
			} else {
				var err error
				unfinished, err = matchesFlexMachine(ctx, reader, op, machineName, nodeName)
				if err != nil {
					return "", fmt.Errorf("match out-of-band MachineOperation %s: %w", op.Name, err)
				}
			}
		}
		if !unfinished {
			continue
		}
		if oldest == nil || operationBefore(op, oldest) {
			oldest = op
		}
	}
	if oldest != nil {
		return oldest.Name, nil
	}
	return "", nil
}

func operationBefore(a, b *machinav1alpha3.MachineOperation) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func markHostSoftRebootWaiting(ctx context.Context, c client.Client, reader client.Reader, name, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op machinav1alpha3.MachineOperation
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !operationPending(op.Status.Phase) {
			return nil
		}
		if op.Status.Phase == machinav1alpha3.OperationPhasePending && op.Status.Message == message {
			return nil
		}
		op.Status.Phase = machinav1alpha3.OperationPhasePending
		op.Status.Message = message
		return c.Status().Update(ctx, &op)
	})
}

func publishHostSoftReboots(ctx context.Context, log *slog.Logger, c client.Client, reader client.Reader, machineName string, currentBootID func() (string, error)) error {
	var operations machinav1alpha3.MachineOperationList
	if err := reader.List(ctx, &operations); err != nil {
		return fmt.Errorf("list MachineOperations: %w", err)
	}
	var bootID string
	for i := range operations.Items {
		op := &operations.Items[i]
		if op.Spec.OperationKind != machinav1alpha3.OperationHostSoftReboot || op.Status.IsTerminal() {
			continue
		}
		target := operationTargetStatus(&op.Status, machineName)
		if target == nil || target.Phase != machinav1alpha3.OperationPhaseInProgress || target.BootID == "" {
			continue
		}
		if bootID == "" {
			var err error
			bootID, err = currentBootID()
			if err != nil {
				return err
			}
		}
		result := agentdaemon.MachineOperationResult[int64]{
			Phase:   machinav1alpha3.OperationPhaseComplete,
			Reason:  "Succeeded",
			Message: "HostSoftReboot completed",
		}
		if target.BootID == bootID {
			result = agentdaemon.MachineOperationResult[int64]{
				Phase:   machinav1alpha3.OperationPhaseFailed,
				Reason:  "RebootNotObserved",
				Message: "host did not reboot after HostSoftReboot was issued",
			}
		}
		if err := finishHostSoftReboot(ctx, c, reader, op.Name, machineName, target.BootID, result); err != nil {
			return fmt.Errorf("report HostSoftReboot %s: %w", op.Name, err)
		}
		log.Info("reported HostSoftReboot", "operation", op.Name, "phase", result.Phase)
	}
	return nil
}
