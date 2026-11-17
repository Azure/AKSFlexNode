package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const (
	testAgentImageV1 = "registry.example/aks-flex-node-agent:v1"
	testAgentImageV2 = "registry.example/aks-flex-node-agent:v2@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestPublishContainerAgentUpgrade(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		runningImage string
		wantPhase    machinav1alpha3.OperationPhase
		wantSelected string
		wantLastGood string
	}{
		"completes when the candidate runs, with its binary in the machine": {
			runningImage: testAgentImageV2,
			wantPhase:    machinav1alpha3.OperationPhaseComplete,
			wantSelected: testAgentImageV2,
			wantLastGood: testAgentImageV1,
		},
		"fails and restores the previous image when the unit restarted on it": {
			runningImage: testAgentImageV1,
			wantPhase:    machinav1alpha3.OperationPhaseFailed,
			wantSelected: testAgentImageV1,
			wantLastGood: testAgentImageV1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
				OperationName:            "upgrade",
				ActiveMachine:            "kube1",
				InitiatingDaemonInstance: "initiator",
				CandidateImage:           testAgentImageV2,
				PreviousImage:            testAgentImageV1,
				SwitchCommitted:          true,
			})
			selected, lastGood := testAgentImageV2, testAgentImageV1
			h.active = true
			h.lastGood = tc.runningImage == testAgentImageV1
			h.rollback = func() { selected, lastGood = testAgentImageV1, testAgentImageV1 }
			if tc.runningImage != testAgentImageV2 {
				h.validation = errors.New("agent restarted on the previous image")
			}

			if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), h.client, h); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if !slices.Equal(h.phases(t), []machinav1alpha3.OperationPhase{tc.wantPhase}) {
				t.Errorf("published phases = %v, want %s", h.phases(t), tc.wantPhase)
			}
			if h.restarts != 0 {
				t.Errorf("restarts = %d, want none", h.restarts)
			}
			if selected != tc.wantSelected || lastGood != tc.wantLastGood {
				t.Errorf("selected %q, last-good %q; want %q, %q", selected, lastGood, tc.wantSelected, tc.wantLastGood)
			}
			if signal, err := h.signals.Read(); err != nil || signal != nil {
				t.Errorf("signal after publishing = %#v, %v", signal, err)
			}
		})
	}
}

func TestPublishContainerAgentUpgradeFailureRestartsIntoLastGoodBeforeClearingSignal(t *testing.T) {
	t.Parallel()

	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "upgrade",
		ActiveMachine:            "kube1",
		InitiatingDaemonInstance: "initiator",
		CandidateImage:           testAgentImageV2,
		PreviousImage:            testAgentImageV1,
		SwitchCommitted:          true,
	})
	selected, lastGood := testAgentImageV2, testAgentImageV1
	h.validation = errors.New("worker binary differs")
	h.active = true
	h.rollback = func() { selected, lastGood = testAgentImageV1, testAgentImageV1 }
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), h.client, h); err != nil {
		t.Fatalf("publish candidate recovery: %v", err)
	}
	if selected != testAgentImageV1 || lastGood != testAgentImageV1 {
		t.Fatalf("rollback selected %q, last-good %q", selected, lastGood)
	}
	if signal, err := h.signals.Read(); err != nil || signal == nil || !signal.RecoveryRequired {
		t.Fatalf("signal before last-good startup = %#v, %v", signal, err)
	}
	if h.restarts != 1 || len(h.phases(t)) != 0 {
		t.Fatalf("restarts = %d, phases = %v", h.restarts, h.phases(t))
	}

	h.instance = "last-good"
	h.lastGood = true
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), h.client, h); err != nil {
		t.Fatalf("publish last-good recovery: %v", err)
	}
	if !slices.Equal(h.phases(t), []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseFailed}) {
		t.Errorf("published phases = %v, want Failed", h.phases(t))
	}
	if h.restarts != 1 {
		t.Errorf("restarts = %d, want no additional restart", h.restarts)
	}
	if signal, err := h.signals.Read(); err != nil || signal != nil {
		t.Errorf("signal after last-good startup = %#v, %v", signal, err)
	}
}

func TestContainerAgentUpgradeInterruptedBeforeStage(t *testing.T) {
	t.Parallel()

	// The recovery unit already recorded the failure; no candidate was selected.
	h := newTestUpgradeHandoff(t, agentdeployment.UpgradeSignal{
		OperationName:            "upgrade",
		ActiveMachine:            "kube1",
		InitiatingDaemonInstance: "initiator",
		Failure:                  "failed repeatedly",
		RecoveryRequired:         true,
	})
	h.lastGood = true
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), h.client, h); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !slices.Equal(h.phases(t), []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseFailed}) || h.restarts != 0 {
		t.Errorf("phases = %v, restarts = %d", h.phases(t), h.restarts)
	}
	if signal, err := h.signals.Read(); err != nil || signal != nil {
		t.Errorf("signal after publishing = %#v, %v", signal, err)
	}
}
