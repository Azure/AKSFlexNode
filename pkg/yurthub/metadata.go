package yurthub

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/retry"
)

const (
	legacyAutonomyAnnotation = "node.beta.openyurt.io/autonomy"
	durationAnnotation       = "node.openyurt.io/autonomy-duration"
	// autonomyDuration "0s" is indefinite autonomy.
	autonomyDuration = "0s"
	// completionKey follows Flex's kubernetes.azure.com/flex-* convention.
	completionKey = "kubernetes.azure.com/flex-yurthub-initialized"
)

// initializeMetadata adds missing Hub labels and annotations once, recording
// completion in the same patch, so later customer edits and removals are kept.
// uid pins it to the Node observed during activation.
func initializeMetadata(ctx context.Context, nodes typedcorev1.NodeInterface, name string, uid types.UID, pool string) error {
	if name == "" || uid == "" {
		return fmt.Errorf("node name and UID are required for hub metadata initialization")
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node, err := nodes.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.UID != uid {
			return fmt.Errorf("node identity changed during hub metadata initialization")
		}
		if node.DeletionTimestamp != nil {
			return fmt.Errorf("cannot initialize hub metadata on a deleting node")
		}
		if completed, exists := node.Annotations[completionKey]; exists {
			if completed != string(node.UID) {
				return fmt.Errorf("hub metadata completion marker does not match node identity")
			}
			return nil
		}
		labels := map[string]string{}
		for key, value := range map[string]string{"openyurt.io/is-edge-worker": "true", "apps.openyurt.io/nodepool": pool} {
			if _, exists := node.Labels[key]; !exists {
				labels[key] = value
			}
		}
		annotations := map[string]string{completionKey: string(node.UID)}
		// Never add the deprecated annotation: the manager lets it override any
		// duration. If either is already set, the customer chose a policy.
		_, hasLegacy := node.Annotations[legacyAutonomyAnnotation]
		_, hasDuration := node.Annotations[durationAnnotation]
		if !hasLegacy && !hasDuration {
			annotations[durationAnnotation] = autonomyDuration
		}
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid": node.UID, "resourceVersion": node.ResourceVersion,
			"labels": labels, "annotations": annotations,
		}})
		if err != nil {
			return fmt.Errorf("encode hub metadata patch: %w", err)
		}
		_, err = nodes.Patch(ctx, node.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("initialize hub node metadata: %w", err)
	}
	return nil
}
