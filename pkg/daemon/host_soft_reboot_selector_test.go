package daemon

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestHostSoftRebootSelectorPeerProgressDelegatesToSDK(t *testing.T) {
	t.Parallel()

	op := softOperation("soft")
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"cohort": "a",
		},
	}
	op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
	op.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: "peer-machine",
			Phase:      machinav1alpha3.OperationPhaseInProgress,
			BootID:     "peer-boot",
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"cohort": "a",
			},
		},
	}
	c := softClient(op, node)
	h := newSoftHarness(t, c, c)

	for range 2 {
		if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
			t.Fatal(err)
		}
	}

	if h.calls != 1 {
		t.Fatalf("local reboot calls = %d, want 1", h.calls)
	}

	current := getSoftOperation(t, c, "soft")
	local := operationTargetStatus(&current.Status, "machine1")
	if local == nil {
		t.Fatal("missing local target alongside the peer")
	}
	if local.Phase != machinav1alpha3.OperationPhaseInProgress {
		t.Fatalf("local phase = %s", local.Phase)
	}
	if local.BootID != "boot1" {
		t.Fatalf("local boot ID = %q", local.BootID)
	}

	peer := operationTargetStatus(&current.Status, "peer-machine")
	if peer == nil {
		t.Fatal("local start removed the peer target")
	}
	if peer.Phase != machinav1alpha3.OperationPhaseInProgress {
		t.Fatalf("peer phase = %s", peer.Phase)
	}
	if peer.BootID != "peer-boot" {
		t.Fatalf("local start changed peer boot ID to %q", peer.BootID)
	}
}
