package daemon

import (
	"context"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemon"
)

const machineOperationResource = "machineoperations"

type discoveryClient interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

type machineOperationTarget struct {
	log      *slog.Logger
	operator NodeOperator
}

func machineOperationReconciler(restCfg *rest.Config, c client.Client, log *slog.Logger, nodeName string, operator NodeOperator) (daemon.MachineOperationRequestReconciler, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create discovery client: %w", err)
	}
	present, err := hasMachineOperationAPI(discoveryClient)
	if err != nil {
		return nil, err
	}
	if !present {
		log.Debug("Machina MachineOperation API not found; using noop machine operation reconciler")
		return daemon.NoopMachineOperationReconciler(), nil
	}

	target := &machineOperationTarget{log: log, operator: operator}
	reconciler, err := daemon.NewMachinaMachineOperationReconciler(
		c,
		nodeName,
		nodeName,
		daemon.MachineOperationHandlers{
			machinav1alpha3.OperationNodeReboot:   target.reconcileNodeReboot,
			machinav1alpha3.OperationAgentUpgrade: target.unsupportedOperation,
			machinav1alpha3.OperationAgentReset:   target.unsupportedOperation,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create MachineOperation reconciler: %w", err)
	}
	log.Info("Machina MachineOperation API found; enabling machine operation reconciler")
	return reconciler, nil
}

func hasMachineOperationAPI(c discoveryClient) (bool, error) {
	resources, err := c.ServerResourcesForGroupVersion(machinav1alpha3.GroupVersion.String())
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("discover Machina MachineOperation API: %w", err)
	}
	for _, resource := range resources.APIResources {
		if resource.Name == machineOperationResource {
			return true, nil
		}
	}
	return false, nil
}

func (t *machineOperationTarget) reconcileNodeReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	if err := store.MarkInProgress(ctx, op, "restarting active nspawn node"); err != nil {
		return ctrl.Result{}, err
	}
	state, err := t.operator.LoadState(ctx)
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}
	active, err := t.operator.FindActiveMachine(ctx, t.log, state)
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}
	if err := t.operator.RestartNode(ctx, t.log, active); err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}
	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseComplete,
		Reason:  "Succeeded",
		Message: "NodeReboot completed",
	})
}

func (t *machineOperationTarget) unsupportedOperation(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseFailed,
		Reason:  "UnsupportedOperation",
		Message: fmt.Sprintf("operation kind %s is not supported by AKS FlexNode daemon", op.Kind),
	})
}

func finishFailedMachineOperation(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation, executionErr error) (ctrl.Result, error) {
	return ctrl.Result{}, store.Finish(ctx, op, daemon.MachineOperationResult[int64]{
		Phase:   machinav1alpha3.OperationPhaseFailed,
		Reason:  "ExecutionFailed",
		Message: executionErr.Error(),
	})
}
