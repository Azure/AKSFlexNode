package daemon

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/noneops"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
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

func TestNoneOperatorForcesNPDRefresh(t *testing.T) {
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
	if err := npd.Start(discardLogger(), cfg, ns).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if err := op.startNPD(t.Context(), discardLogger(), cfg, ns); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls, "systemctl restart "+npd.HostUnitNPD) {
		t.Fatalf("operator did not force NPD to reload its binary: %v", calls)
	}
}

func TestNoneRestartUsesAppliedArtifactVersion(t *testing.T) {
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
			if p.ArtifactVersion != state.NPDVersion || p.Generation != state.AppliedSettingsVersion {
				t.Error("restart replaced the applied identity with desired artifacts")
			}
			return fakeNoneNode{events: &events}
		},
		startNPD: func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error {
			events = append(events, "npd")
			return nil
		},
	}
	if err := op.RestartNode(t.Context(), discardLogger()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(events, []string{"recover", "restart", "npd"}) {
		t.Fatalf("restart events = %v", events)
	}
	if state.NPDVersion != "v1.35.1" {
		t.Fatal("soft reboot changed the applied artifact version")
	}
}
