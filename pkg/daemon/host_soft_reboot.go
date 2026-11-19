package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type flexHostSoftRebootTarget struct {
	machineName string
	nodeName    string
}

func (t flexHostSoftRebootTarget) Reference() string {
	return t.machineName
}

func (t flexHostSoftRebootTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return 0, nil
}

func (t flexHostSoftRebootTarget) MatchSelector(
	ctx context.Context,
	reader client.Reader,
	_ machinav1alpha3.OperationKind,
	selector labels.Selector,
) (bool, error) {
	var node corev1.Node
	if err := reader.Get(ctx, client.ObjectKey{Name: t.nodeName}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get Node %s for selector match: %w", t.nodeName, err)
	}

	return selector.Matches(labels.Set(node.Labels)), nil
}

func (h *machineOperationHandlers) reconcileHostSoftReboot(
	ctx context.Context,
	store agentdaemon.MachineOperationStore[int64],
	op agentdaemon.MachineOperation,
) (ctrl.Result, error) {
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

	return h.softReboot.Reconcile(ctx, store, op)
}
