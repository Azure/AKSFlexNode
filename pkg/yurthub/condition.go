package yurthub

import (
	"context"
	"encoding/json"
	"errors"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

// hubConditionType reports DT state alongside kubelet and NPD conditions.
const hubConditionType corev1.NodeConditionType = "DisconnectTolerance"

// nodeConditionWriter patches one Node condition, merged by type so kubelet's
// and NPD's conditions are kept. The daemon credential is the node identity,
// which may patch its own status.
func nodeConditionWriter(nodes typedcorev1.NodeInterface, name string) func(context.Context, corev1.NodeCondition) error {
	return func(ctx context.Context, condition corev1.NodeCondition) error {
		patch, err := json.Marshal(map[string]any{
			"status": map[string]any{
				"conditions": []corev1.NodeCondition{condition},
			},
		})
		if err != nil {
			return err
		}

		_, err = nodes.PatchStatus(ctx, name, patch)
		return err
	}
}

// reportCondition writes the condition when it differs from the Node's, or when the
// Node's heartbeat is older than a.heartbeat (or in the future). A steady Disabled
// condition gets no heartbeat, so nodes with DT off cost no extra Node writes or
// Machine reads. Comparing against the live Node (not memory) repairs external edits
// and keeps the transition time across agent restarts. A failed write is logged,
// never fails the reconcile, and is retried on the next read.
func (a *Reconciler) reportCondition(ctx context.Context, current *corev1.NodeCondition, condition corev1.NodeCondition) {
	now := a.now()
	if current != nil && current.Status == condition.Status && current.Reason == condition.Reason && current.Message == condition.Message {
		if condition.Reason == hubReasonDisabled {
			return
		}
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

const hubReasonDisabled = "Disabled"

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
		condition.Status, condition.Reason, condition.Message = corev1.ConditionFalse, hubReasonDisabled, "Disconnect tolerance is not enabled."
	case errors.Is(err, ErrUnsupportedWorker):
		condition.Status, condition.Reason, condition.Message = corev1.ConditionFalse, "Unsupported", "Disconnect tolerance is not supported on this node."+see
	case errors.Is(err, ErrPackageDownload):
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
