package daemon

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestNoneNPDVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, configured, offline, want string
	}{
		{name: "default", want: npd.DefaultVersion},
		{name: "explicit", configured: "v1.36.0", want: "v1.36.0"},
		{name: "offline", configured: "v1.36.0", offline: "/artifacts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &config.Config{
				Npd:       config.NPDConfig{Version: tc.configured},
				Bootstrap: config.BootstrapConfig{OfflineArtifacts: config.OfflineArtifactsConfig{Source: tc.offline}},
			}
			if got := noneNPDVersion(cfg); got != tc.want {
				t.Fatalf("NPD version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNoneOperatorReconcilesNPD(t *testing.T) {
	t.Parallel()
	var events []string
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}}
	state := &State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}
	operator, err := newNodeOperator(t.Context(), cfg, &noneTestStore{events: &events, state: state})
	if err != nil {
		t.Fatal(err)
	}
	op, ok := operator.(*noneNodeOperator)
	if !ok {
		t.Fatal("operator is not host-native")
	}
	var calls []string
	ns := &goalstates.NodeStart{
		RootOverride: none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "node"},
		NodeName:     "worker-1",
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if len(args) > 1 && args[1] == "show" {
				return "ActiveState=active\nSubState=running\nMainPID=123\n", nil
			}
			return "active", nil
		},
	}
	if err := npd.Start(discardLogger(), cfg, ns.Root(), ns, npd.StartOptions{
		UnitName: npd.HostUnitNPD, BinaryPath: ns.Root().Path(npd.BinaryPath),
	}).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	unit := ns.Root().Path("/etc/systemd/system/" + npd.HostUnitNPD)
	before, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "ExecStart="+ns.Root().Path(npd.BinaryPath)+" ") {
		t.Fatalf("NPD does not execute the versioned root's binary: %s", content)
	}
	calls = nil
	if err := op.startNPD(t.Context(), discardLogger(), cfg, ns); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl is-active " + npd.HostUnitNPD}
	if !slices.Equal(calls, want) {
		t.Fatalf("operator NPD commands = %v, want %v", calls, want)
	}
	after, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(unit); err != nil || string(got) != string(content) || !os.SameFile(before, after) {
		t.Fatalf("NPD reconciliation replaced an unchanged unit: %v", err)
	}
}

func TestNoneRestartUsesAppliedNPDVersion(t *testing.T) {
	t.Parallel()
	var events []string
	state := &State{
		Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "none"},
		AppliedSettingsVersion: "42", NPDVersion: "v1.35.1",
	}
	store := &noneTestStore{events: &events, state: state}
	op := &noneNodeOperator{
		nspawnNodeOperator: &nspawnNodeOperator{
			cfg: &config.Config{Npd: config.NPDConfig{Version: "v1.36.0"}}, state: store,
		},
		newNode: func(p noneops.Params) noneNode {
			if p.Config != state.NodeConfig {
				t.Error("restart replaced the applied configuration")
			}
			return fakeNoneNode{events: &events}
		},
		dependencies: func(context.Context, *slog.Logger, *config.Config) error { return nil },
		installNPD: func(_ context.Context, _ *slog.Logger, cfg *config.Config, _ *goalstates.MachineGoalState) error {
			if cfg.Npd.Version != state.NPDVersion {
				t.Error("recovery installed desired NPD instead of applied NPD")
			}
			events = append(events, "artifacts")
			return nil
		},
		startNPD: func(_ context.Context, _ *slog.Logger, cfg *config.Config, _ *goalstates.NodeStart) error {
			if cfg.Npd.Version != state.NPDVersion {
				t.Error("restart selected desired NPD instead of applied NPD")
			}
			events = append(events, "npd")
			return nil
		},
	}
	if err := op.RestartNode(t.Context(), discardLogger()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"recover", "artifacts", "npd", "restart", "npd"}) {
		t.Fatalf("restart events = %v", events)
	}
	if state.NPDVersion != "v1.35.1" {
		t.Fatal("soft reboot changed the applied artifact version")
	}
}
