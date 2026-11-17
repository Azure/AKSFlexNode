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
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestHostNativeNPD(t *testing.T) {
	t.Parallel()
	root := none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "v1"}
	var calls []string
	ns := &goalstates.NodeStart{
		RootOverride: root,
		NodeName:     "flex-worker",
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			calls = append(calls, strings.Join(args, " "))
			return "active", nil
		},
	}
	cfg := &config.Config{}
	task := Start(slog.New(slog.DiscardHandler), cfg, ns.Root(), ns, StartOptions{})
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	path := root.Path(filepath.Join("/etc/systemd/system", HostUnitNPD))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "/opt/unbounded/node/current/bin/node-problem-detector") || !strings.Contains(string(content), "flex-worker") {
		t.Fatalf("incorrect NPD service: %s", content)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "systemctl ") || !strings.Contains(call, HostUnitNPD) && call != "systemctl daemon-reload" {
			t.Fatalf("unexpected host NPD command %s", call)
		}
	}
	assertRoot := Download(slog.New(slog.DiscardHandler), cfg, root)
	download, ok := assertRoot.(*downloadTask)
	if !ok || download.root != root {
		t.Fatal("NPD download does not target the versioned root")
	}
}

func TestHostNativeNPDForcedRestart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		force, active bool
		want          []string
	}{
		{
			name: "unchanged active start", active: true,
			want: []string{"systemctl is-active " + HostUnitNPD},
		},
		{
			name: "unchanged active restart", force: true, active: true,
			want: []string{"systemctl is-active " + HostUnitNPD, "systemctl restart " + HostUnitNPD},
		},
		{
			name: "inactive restart starts service", force: true,
			want: []string{"systemctl is-active " + HostUnitNPD, "systemctl daemon-reload", "systemctl enable --now " + HostUnitNPD},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := none.Root{HostRoot: t.TempDir(), Layout: none.Layout{Base: t.TempDir()}, Version: "v1"}
			var calls []string
			active := true
			ns := &goalstates.NodeStart{
				RootOverride: root, NodeName: "flex-worker",
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					calls = append(calls, strings.Join(args, " "))
					if len(args) > 1 && args[1] == "is-active" && !active {
						return "inactive", errors.New("inactive")
					}
					return "active", nil
				},
			}
			log, cfg := slog.New(slog.DiscardHandler), &config.Config{}
			if err := Start(log, cfg, ns.Root(), ns, StartOptions{}).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			path := root.Path(filepath.Join("/etc/systemd/system", HostUnitNPD))
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				calls, active = nil, tc.active
				task := Start(log, cfg, ns.Root(), ns, StartOptions{})
				if tc.force {
					task = Start(log, cfg, ns.Root(), ns, StartOptions{ForceRestart: true})
				}
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(calls, tc.want) {
					t.Fatalf("NPD commands = %v, want %v", calls, tc.want)
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
		})
	}
}
