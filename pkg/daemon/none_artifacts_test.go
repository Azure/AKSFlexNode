package daemon

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestNoneOperatorReconcilesNPD(t *testing.T) {
	t.Parallel()
	var events []string
	cfg := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: "none"}}
	state := &runtimeprovider.State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}
	operator, err := mustNodeProvider(t, cfg.Bootstrap.Isolation).NewOperator(t.Context(), cfg, &noneTestStore{events: &events, state: state})
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
	if err := npd.Start(discardLogger(), cfg, ns.Root(), ns, isolation.ModeNone).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	unit := ns.Root().Path("/etc/systemd/system/node-problem-detector.service")
	before, err := os.Stat(unit)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "ExecStart="+ns.Root().Path("opt/aks-flex-node/bin/node-problem-detector")+" ") {
		t.Fatalf("NPD does not execute the versioned root's binary: %s", content)
	}
	calls = nil
	if err := op.startNPD(t.Context(), discardLogger(), cfg, ns); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl is-active node-problem-detector.service"}
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
