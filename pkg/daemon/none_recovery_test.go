package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestNoneBaseSoftRestartDoesNotRepave(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	if err := f.op.recover(t.Context(), discardLogger()); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(f.params.HostRoot, goalstates.KubeletKubeconfigPath)
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentials, []byte("client credentials"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls, cleanups := len(f.calls), f.cleanups
	for range 2 {
		if err := f.op.RestartNode(t.Context(), discardLogger()); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"systemctl restart " + none.UnitContainerd,
		"systemctl restart " + none.UnitKubelet,
		"systemctl restart " + none.UnitContainerd,
		"systemctl restart " + none.UnitKubelet,
	}
	if !slices.Equal(f.calls[calls:], want) || f.cleanups != cleanups {
		t.Fatalf("soft restart changed pod/network lifecycle: %v", f.calls[calls:])
	}
	if data, err := os.ReadFile(credentials); err != nil || string(data) != "client credentials" {
		t.Fatalf("soft restart replaced credentials: %v", err)
	}
	if f.store.state.AppliedSettingsVersion != "41" || f.store.state.NodeConfig.Kubelet.Labels[runtimeGenerationLabel] != "source-nonce" {
		t.Fatal("soft restart changed applied settings or registration nonce")
	}
	f.assertCommitted(t)
	f.assertAppliedRuntime(t, f.store.state)
}

func TestNoneRecoveryRestoresOfflineNPDPolicy(t *testing.T) {
	t.Parallel()
	f := newCompletionFixture(t)
	f.store.state.NPDVersion = ""
	f.store.state.NodeConfig.OfflineArtifacts = &agentconfig.AgentOfflineArtifacts{
		Source: "https://offline-user:offline-secret@example.invalid/artifacts?sig=offline-secret",
	}
	// Target is online, but it never becomes the applied configuration.
	f.store.failSave = true
	if _, err := f.op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "42", KubernetesVersion: "1.34.1"}); err == nil {
		t.Fatal("expected persistence failure")
	}
	unit := filepath.Join(f.params.HostRoot, "etc/systemd/system", npd.HostUnitNPD)
	if err := os.WriteFile(unit, []byte("owned host NPD unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.store.failSave = false
	f.op.installNPD = installNoneNPD
	f.op.startNPD = func(ctx context.Context, log *slog.Logger, cfg *config.Config, ns *goalstates.NodeStart) error {
		return npd.Start(log, cfg, ns.Root(), ns, npd.StartOptions{
			ForceRestart: true, UnitName: npd.HostUnitNPD, BinaryPath: ns.Root().Path(npd.BinaryPath),
		}).Do(ctx)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	calls := len(f.calls)
	if _, err := f.op.ResumeRepave(t.Context(), log, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl stop " + npd.HostUnitNPD, "systemctl disable " + npd.HostUnitNPD}
	if !slices.Equal(f.calls[len(f.calls)-2:], want) || len(f.calls) <= calls {
		t.Fatalf("recovery did not stop/disable offline source NPD: %v", f.calls[calls:])
	}
	if strings.Contains(logs.String(), "offline-secret") || strings.Contains(logs.String(), "offline-user") {
		t.Fatal("recovery logged artifact credentials")
	}
	if f.store.state.AppliedSettingsVersion != "41" || f.store.state.NPDVersion != "" {
		t.Fatal("recovery published failed online target")
	}
	f.assertCommitted(t)
}
