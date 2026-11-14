package npd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

func TestContainerNPDUsesRootAndExec(t *testing.T) {
	t.Parallel()
	root := container.Root{SlotName: "kube2", Dir: t.TempDir(), HostRoot: t.TempDir()}
	decoy := t.TempDir()
	var calls []string
	ns := &goalstates.NodeStart{
		MachineDir: decoy, RootOverride: root, InContainer: true, NodeName: "container-worker",
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			if len(args) > 1 && args[1] == "show" {
				return runningNPDStatus, nil
			}
			return "active", nil
		},
	}
	cfg, log := &config.Config{}, slog.New(slog.DiscardHandler)
	for range 2 {
		if err := Start(log, cfg, ns).Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(root.Path("/etc/systemd/system/" + systemdUnitNPD))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ExecStart=/usr/local/bin/node-problem-detector") || !strings.Contains(string(data), "container-worker") {
		t.Fatalf("incorrect container service: %s", data)
	}
	if entries, err := os.ReadDir(decoy); err != nil || len(entries) != 0 {
		t.Fatalf("NPD wrote through MachineDir: %v", err)
	}
	calls = nil
	if err := Restart(log, cfg, ns).Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls, "systemctl restart "+systemdUnitNPD) {
		t.Fatalf("NPD was not restarted through the container executor: %v", calls)
	}
	// The download and rendered command must name the same binary. A local
	// version probe avoids downloading external artifacts in this test.
	binary := root.Path("/usr/local/bin/node-problem-detector")
	if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\necho "+DefaultVersion+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := DownloadToRoot(log, cfg, root).Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
