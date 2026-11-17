package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runtimeprovider "github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// TestResetNodeCleansUpLocalDNS pins that reset runs the library's full
// network cleanup. It used to call the interface and route cleanups directly
// and skip the LocalDNS one, so every reset of a LocalDNS node left the unit,
// the nft table, the dummy interface and the helper on the host.
func TestResetNodeCleansUpLocalDNS(t *testing.T) {
	t.Parallel()

	name := mustNodeProvider(t, "nspawn").ResetNode(slog.New(slog.DiscardHandler)).Name()

	for _, want := range []string{
		"cleanup-localdns-rules",
		"remove-network-interfaces",
		"cleanup-routes",
		"remove-host-helpers",
	} {
		if !strings.Contains(name, want) {
			t.Fatalf("ResetNode does not run %s: %s", want, name)
		}
	}
}

// TestHostHelperPaths covers the library helpers reset removes. Reset does not
// migrate the host root, so the legacy root is swept too, including the LocalDNS
// helper, which CleanupNetwork removes under the resolved root only.
func TestHostHelperPaths(t *testing.T) {
	t.Parallel()

	legacy := goalstates.LegacyHostPaths()

	tests := []struct {
		name     string
		resolved goalstates.HostPaths
		want     []string
	}{
		{
			name:     "host root also sweeps the legacy root",
			resolved: goalstates.HostPaths{Root: "/opt/unbounded/agent", NSpawnLifecycleBinary: "/opt/unbounded/agent/bin/unbounded-agent-nspawn-lifecycle"},
			want: []string{
				"/opt/unbounded/agent/bin/unbounded-agent-nspawn-lifecycle",
				"/usr/local/bin/unbounded-agent-nspawn-lifecycle",
				"/usr/local/libexec/unbounded-localdns-network",
			},
		},
		{
			name:     "migrated host sweeps the legacy root once",
			resolved: legacy,
			want:     []string{"/usr/local/bin/unbounded-agent-nspawn-lifecycle"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := hostHelperPathsFor(tt.resolved, legacy)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("hostHelperPathsFor(%q) = %v, want %v", tt.resolved.Root, got, tt.want)
			}
		})
	}
}

// TestRemoveFilesTaskIsIdempotent runs the removal against a temp tree twice.
// Reset can be retried after a partial failure, so a second pass over files
// that are already gone must succeed.
func TestRemoveFilesTaskIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	task := &removeFilesTask{
		name:  "test",
		log:   slog.New(slog.DiscardHandler),
		paths: []string{present, filepath.Join(dir, "absent")},
	}

	for pass := 1; pass <= 2; pass++ {
		if err := task.Do(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	if _, err := os.Lstat(present); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was not removed: %v", err)
	}
}

func TestResetNodeAppliedStateRouting(t *testing.T) {
	t.Parallel()
	noneState := func() *runtimeprovider.State {
		return &runtimeprovider.State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "none"}}
	}
	for _, tc := range []struct {
		name    string
		state   func() *runtimeprovider.State
		damage  string
		want    string
		wantErr bool
	}{
		{name: "missing state keeps legacy reset", want: "nspawn"},
		{name: "legacy nspawn state", state: func() *runtimeprovider.State { return &runtimeprovider.State{ActiveMachine: "kube1"} }, want: "nspawn"},
		{name: "explicit nspawn state", state: func() *runtimeprovider.State { return &runtimeprovider.State{Isolation: isolation.ModeNSpawn} }, want: "nspawn"},
		{name: "none state", state: noneState, want: "none"},
		{name: "container state", state: func() *runtimeprovider.State { return containerTestState("kube1", "42") }, want: "container"},
		{name: "none without config", state: func() *runtimeprovider.State { return &runtimeprovider.State{Isolation: isolation.ModeNone} }, wantErr: true},
		{name: "none with container config", state: func() *runtimeprovider.State {
			return &runtimeprovider.State{Isolation: isolation.ModeNone, NodeConfig: &agentconfig.AgentConfig{Isolation: "container"}}
		}, wantErr: true},
		{name: "container without config", state: func() *runtimeprovider.State { return &runtimeprovider.State{Isolation: isolation.ModeContainer} }, wantErr: true},
		{name: "container without opt in", state: func() *runtimeprovider.State {
			s := containerTestState("kube1", "42")
			s.NodeConfig.Experimental = nil
			return s
		}, wantErr: true},
		{name: "container with mismatched slot", state: func() *runtimeprovider.State {
			s := containerTestState("kube1", "42")
			s.ActiveMachine = "kube2"
			return s
		}, wantErr: true},
		{name: "unsupported mode", state: func() *runtimeprovider.State { return &runtimeprovider.State{Isolation: "unknown"} }, wantErr: true},
		{name: "unreadable state", damage: "unreadable", wantErr: true},
		{name: "malformed state", damage: "malformed", wantErr: true},
		{name: "tampered none state", state: noneState, damage: "checksum", wantErr: true},
		{name: "missing legacy checksum", state: func() *runtimeprovider.State { return &runtimeprovider.State{ActiveMachine: "kube1"} }, damage: "sidecar", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			store, err := newFileStateStore(filepath.Join(root, "daemon-state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.state != nil {
				if err := store.Save(t.Context(), tc.state()); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.damage {
			case "unreadable":
				if err := os.Mkdir(store.path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "malformed":
				putNoneResetFile(t, root, stateFileName, "{")
			case "checksum":
				data, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				putNoneResetFile(t, root, stateFileName, string(bytes.Replace(data, []byte(`"none"`), []byte(`"container"`), 1)))
			case "sidecar":
				if err := os.Remove(store.checksumPath()); err != nil {
					t.Fatal(err)
				}
			}
			ipam := putNoneResetFile(t, root, "var/lib/cni/networks/site/10.0.0.2", "site-owned")
			config := putNoneResetFile(t, root, "etc/cni/net.d/site.conflist", "site-owned")
			provider, err := discoverResetProvider(t.Context(), store.Load, discardLogger(), nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reset=%v, wantErr=%v", err, tc.wantErr)
			}
			if tc.wantErr && provider != nil || !tc.wantErr && string(provider.Mode()) != tc.want {
				t.Fatalf("reset provider=%v, want %q", provider, tc.want)
			}
			if !tc.wantErr && provider.Mode() == isolation.ModeNone {
				if err := hostnode.ResetCNIState(root); err != nil {
					t.Fatal(err)
				}
			}
			for _, path := range []string{ipam, config} {
				if tc.want == "none" {
					assertNoneResetAbsent(t, "", path)
				} else if data, err := os.ReadFile(path); err != nil || string(data) != "site-owned" {
					t.Fatalf("unowned %s changed: %q, %v", path, data, err)
				}
			}
		})
	}
}
