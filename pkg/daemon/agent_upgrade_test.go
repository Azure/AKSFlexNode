package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

func TestNewDaemonInstanceID(t *testing.T) {
	t.Parallel()

	first, err := newDaemonInstanceID()
	if err != nil {
		t.Fatalf("newDaemonInstanceID: %v", err)
	}
	second, err := newDaemonInstanceID()
	if err != nil {
		t.Fatalf("newDaemonInstanceID: %v", err)
	}
	if first == "" || second == "" || first == second {
		t.Fatalf("instance IDs = %q, %q", first, second)
	}
}

func TestParseAgentUpgradeRequest(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		parameters map[string]string
		wantErr    string
	}{
		"valid": {
			parameters: map[string]string{
				agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz?sig=secret",
				agentdeployment.UpgradeSHA256Parameter:      strings.Repeat("a", 64),
			},
		},
		"missing URL": {
			parameters: map[string]string{agentdeployment.UpgradeSHA256Parameter: strings.Repeat("a", 64)},
			wantErr:    agentdeployment.UpgradeDownloadURLParameter,
		},
		"HTTP URL": {
			parameters: map[string]string{
				agentdeployment.UpgradeDownloadURLParameter: "http://example.com/agent.tar.gz",
				agentdeployment.UpgradeSHA256Parameter:      strings.Repeat("a", 64),
			},
		},
		"missing digest": {
			parameters: map[string]string{agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz"},
			wantErr:    agentdeployment.UpgradeSHA256Parameter,
		},
		"blank digest": {
			parameters: map[string]string{
				agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
				agentdeployment.UpgradeSHA256Parameter:      " \t\n",
			},
			wantErr: agentdeployment.UpgradeSHA256Parameter,
		},
		"invalid digest": {
			parameters: map[string]string{
				agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
				agentdeployment.UpgradeSHA256Parameter:      "bad",
			},
			wantErr: "64 hexadecimal",
		},
		// Which URLs an agent takes is its executor's to check; see
		// TestAgentUpgradeExecutorsTakeTheirOwnKindOfRequest.
		"an image": {
			parameters: map[string]string{
				agentdeployment.UpgradeDownloadURLParameter: "oci://registry.example/aks-flex-node-agent",
				agentdeployment.UpgradeSHA256Parameter:      "sha256:" + strings.Repeat("a", 64),
			},
		},
		"an image without its digest": {
			parameters: map[string]string{agentdeployment.UpgradeDownloadURLParameter: "oci://registry.example/aks-flex-node-agent"},
			wantErr:    agentdeployment.UpgradeSHA256Parameter,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseAgentUpgradeRequest(tt.parameters)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("parseAgentUpgradeRequest: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestHostAgentUpgradeExecutorRecordPendingIsIdempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent-upgrade.json")
	signals := agentdeployment.NewUpgradeSignalStore(path)
	store := &testStateStore{state: &State{ActiveMachine: "kube1"}}
	if err := recordPendingAgentUpgrade(t.Context(), signals, store, "operation-1", "instance"); err != nil {
		t.Fatalf("first RecordPending: %v", err)
	}
	if err := recordPendingAgentUpgrade(t.Context(), signals, store, "operation-1", "instance"); !errors.Is(err, errAgentUpgradeAlreadyPending) {
		t.Fatalf("second RecordPending error = %v, want errAgentUpgradeAlreadyPending", err)
	}
	if err := recordPendingAgentUpgrade(t.Context(), signals, store, "operation-2", "instance"); err == nil || !strings.Contains(err.Error(), "operation-1") {
		t.Fatalf("competing RecordPending error = %v", err)
	}
}
