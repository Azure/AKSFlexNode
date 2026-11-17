package npd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// wantKubeconfigPath is the path the kubelet actually writes its kubeconfig to.
// The historical bug used a doubled segment ("/var/lib/kubelet/kubelet/kubeconfig"),
// which made node-problem-detector panic on startup and crash-loop forever:
//
//	panic: stat /var/lib/kubelet/kubelet/kubeconfig: no such file or directory
const wantKubeconfigPath = "/var/lib/kubelet/kubeconfig"

// TestCanonicalKubeletKubeconfigPath guards the value of the shared library
// constant so the doubled-segment typo cannot reappear from a dependency bump.
func TestCanonicalKubeletKubeconfigPath(t *testing.T) {
	t.Parallel()
	if goalstates.KubeletKubeconfigPath != wantKubeconfigPath {
		t.Fatalf("goalstates.KubeletKubeconfigPath = %q, want %q",
			goalstates.KubeletKubeconfigPath, wantKubeconfigPath)
	}
}

// TestRenderedNPDUnitUsesCanonicalKubeconfig drives Start() through the real
// service-file rendering and asserts on the kubeconfig path that ends up in the
// node-problem-detector systemd unit's ExecStart. Asserting on the rendered unit
// (the externally observable artifact) rather than internal fields keeps the
// test stable across refactors while still exercising the Start() wiring.
func TestRenderedNPDUnitUsesCanonicalKubeconfig(t *testing.T) {
	t.Parallel()
	machineDir := t.TempDir()
	nodeStart := &goalstates.NodeStart{
		MachineDir:  machineDir,
		MachineName: "kube1",
		NodeName:    "vm-e2e-token-1781659839",
		Kubelet: goalstates.Kubelet{
			APIServer: "https://example.hcp.westus.azmk8s.io:443",
		},
	}
	task, ok := Start(slog.Default(), &config.Config{}, nodeStart.Root(), nodeStart, isolation.ModeNSpawn).(*startTask)
	if !ok {
		t.Fatalf("Start did not return *startTask")
	}
	if _, err := task.ensureServiceFile(); err != nil {
		t.Fatalf("ensureServiceFile: %v", err)
	}

	unitPath := filepath.Join(machineDir, "etc/systemd/system", UnitName)
	data, err := os.ReadFile(unitPath) //nolint:gosec // path built from test TempDir
	if err != nil {
		t.Fatalf("read rendered unit: %v", err)
	}
	rendered := string(data)

	if !strings.Contains(rendered, "ExecStart=/usr/local/bin/node-problem-detector ") {
		t.Fatalf("rendered unit missing canonical NPD binary path:\n%s", rendered)
	}
	if !strings.Contains(rendered, "auth="+wantKubeconfigPath) {
		t.Fatalf("rendered unit missing auth=%s:\n%s", wantKubeconfigPath, rendered)
	}
	if strings.Contains(rendered, "kubelet/kubelet") {
		t.Fatalf("rendered unit contains doubled 'kubelet' segment:\n%s", rendered)
	}
	if !strings.Contains(rendered, "--hostname-override=vm-e2e-token-1781659839") {
		t.Fatalf("rendered unit missing hostname override:\n%s", rendered)
	}
}

func TestNPDUsesNodeRootAndExec(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeContainer, isolation.ModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var root isolation.NodeRoot = isolation.DirRoot{Dir: dir}
			binary := BinaryPath
			var calls []string
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), MachineName: "kube2", NodeName: "flex-worker",
				RootOverride: none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "decoy"},
				Kubelet:      goalstates.Kubelet{APIServer: "https://example.test:443"},
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					calls = append(calls, strings.Join(args, " "))
					return "active", nil
				},
			}
			switch mode {
			case isolation.ModeContainer:
				root = container.Root{SlotName: "kube2", Dir: dir, HostRoot: t.TempDir()}
				ns.InContainer = true
			case isolation.ModeNone:
				root = none.Root{HostRoot: dir, Layout: none.Layout{Base: t.TempDir()}, Version: "v1"}
				binary = root.Path(BinaryPath)
			}
			cfg, log := &config.Config{}, slog.New(slog.DiscardHandler)
			path := root.Path("/etc/systemd/system/" + UnitName)
			if err := Start(log, cfg, root, ns, mode).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			want := []string{"systemctl is-active " + UnitName, "systemctl restart " + UnitName}
			if !slices.Equal(calls, want) {
				t.Fatalf("initial NPD commands = %v, want %v", calls, want)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{
				"ExecStart=" + binary + " ", "auth=" + wantKubeconfigPath,
				"--hostname-override=flex-worker", "https://example.test:443", npdConfigPath,
			} {
				if !strings.Contains(string(content), fragment) {
					t.Fatalf("NPD unit missing %q:\n%s", fragment, content)
				}
			}
			for range 2 {
				calls = nil
				if err := Start(log, cfg, root, ns, mode).Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				if want := []string{"systemctl is-active " + UnitName}; !slices.Equal(calls, want) {
					t.Fatalf("unchanged NPD commands = %v, want %v", calls, want)
				}
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, content) || !os.SameFile(before, after) {
				t.Fatalf("unchanged NPD unit was replaced: %v", err)
			}
			if mode != isolation.ModeNone && strings.Contains(string(content), root.Path(BinaryPath)) {
				t.Fatalf("agent-side root leaked into guest ExecStart:\n%s", content)
			}
			for _, decoy := range []string{ns.MachineDir, ns.Root().Path("/etc/systemd/system")} {
				if entries, err := os.ReadDir(decoy); !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
					t.Fatalf("NPD wrote through NodeStart instead of supplied NodeRoot: %s: %v", decoy, err)
				}
			}
		})
	}
}

func TestNPDSystemdActions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, failAction  string
		inactive, restart bool
		want              []string
	}{
		{name: "active unchanged", want: []string{"is-active"}},
		{name: "active updated", restart: true, want: []string{"is-active", "restart"}},
		{name: "inactive unchanged", inactive: true, want: []string{"is-active", "daemon-reload", "enable --now"}},
		{name: "inactive updated", inactive: true, restart: true, want: []string{"is-active", "daemon-reload", "enable --now"}},
		{name: "reload failure", inactive: true, failAction: "daemon-reload", want: []string{"is-active", "daemon-reload"}},
		{name: "start failure", inactive: true, failAction: "enable", want: []string{"is-active", "daemon-reload", "enable --now"}},
		{name: "restart failure", restart: true, failAction: "restart", want: []string{"is-active", "restart"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, log := t.Context(), slog.New(slog.DiscardHandler)
			failure := errors.New("systemctl failed")
			var calls []string
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), MachineName: "kube1",
				ExecOverride: func(gotCtx context.Context, gotLog *slog.Logger, args ...string) (string, error) {
					if gotCtx != ctx || gotLog != log {
						t.Fatal("NPD did not propagate the context and logger to Exec")
					}
					calls = append(calls, strings.Join(args, " "))
					if args[1] == tc.failAction {
						return "", failure
					}
					if args[1] == "is-active" && tc.inactive {
						return "inactive", errors.New("inactive")
					}
					return "active", nil
				},
			}
			task, ok := Start(log, &config.Config{}, ns.Root(), ns, isolation.ModeNSpawn).(*startTask)
			if !ok {
				t.Fatal("Start did not return *startTask")
			}
			err := task.ensureSystemdUnit(ctx, tc.restart)
			if tc.failAction != "" {
				if !errors.Is(err, failure) {
					t.Fatalf("error = %v, want wrapped systemctl failure", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var want []string
			for _, action := range tc.want {
				command := "systemctl " + action
				if action != "daemon-reload" {
					command += " " + UnitName
				}
				want = append(want, command)
			}
			if !slices.Equal(calls, want) {
				t.Fatalf("NPD commands = %v, want %v", calls, want)
			}
		})
	}
}
