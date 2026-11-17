package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestNoneSDKLifecycleAndAppliedConfigRecovery(t *testing.T) {
	t.Parallel()
	for _, persisted := range []bool{false, true} {
		name := "save failed"
		if persisted {
			name = "saved before SDK commit"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, units, proc := t.TempDir(), t.TempDir(), t.TempDir()
			layout := none.Layout{Base: filepath.Join(t.TempDir(), "node")}
			snapshot := filepath.Join(t.TempDir(), "snapshot")
			active := map[string]bool{}
			var mu sync.Mutex
			run := func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
				mu.Lock()
				defer mu.Unlock()
				if command != "systemctl" || len(args) < 2 {
					return "", nil
				}
				unit := args[len(args)-1]
				switch args[0] {
				case "is-active":
					if active[unit] {
						return "active", nil
					}
					return "inactive", errors.New("inactive")
				case "stop":
					active[unit] = false
				case "enable", "restart":
					active[unit] = true
					if unit == none.UnitKubelet {
						path := filepath.Join(root, "var/lib/kubelet/kubeconfig")
						if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
							if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
								return "", err
							}
							if err := os.WriteFile(path, []byte("client certificate identity"), 0o600); err != nil {
								return "", err
							}
						}
					}
				}
				return "", nil
			}
			goalState := func(version string) *goalstates.MachineGoalState {
				gs := hostnodetest.GoalState("kube1", false)
				gs.RootFS.KubernetesVersion = version
				return gs
			}
			p := newNoneProvider()
			p.newNode = func(params noneops.Params) *noneops.Node {
				params.GoalState = goalState(params.Config.Cluster.Version)
				if params.Previous != nil {
					params.PreviousGoalState = goalState(params.Previous.Cluster.Version)
				}
				params.Overrides = noneops.Overrides{Layout: layout, SystemdDir: units, Proc: proc, Run: run,
					Download: func(context.Context) error { return nil }, WaitKubelet: func(context.Context) error { return nil }}
				params.Hooks = hostnode.Hooks{HostRoot: root, ConfigSnapshotDir: snapshot,
					SetupNetwork: func(context.Context) error { return nil }, CleanupNetwork: func(context.Context) error { return nil }}
				return noneops.New(params)
			}
			staged := 0
			p.stageArchives = func(context.Context, *slog.Logger, *goalstates.ContainerImageArchiveStaging) error {
				staged++
				return nil
			}
			previous := Node{Slot: noneops.Slot, Config: &agentconfig.AgentConfig{Isolation: string(isolation.ModeNone), Cluster: agentconfig.AgentClusterConfig{Version: "1.34.1"}}}
			target := Node{Slot: noneops.Slot, Config: previous.Config.DeepCopy()}
			target.Config.Cluster.Version = "1.35.0"
			flex := &config.Config{Bootstrap: config.BootstrapConfig{Isolation: isolation.ModeNone, OfflineArtifacts: config.OfflineArtifactsConfig{Source: "test-offline-source"}}}
			log := slog.New(slog.DiscardHandler)
			initial := p.node(log, previous, Node{})
			if err := initial.Provision(t.Context()); err != nil {
				t.Fatal(err)
			}
			cd, kl, err := initial.Configure(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := initial.Start(t.Context(), false, cd, kl); err != nil {
				t.Fatal(err)
			}
			oldVersion, err := layout.Current()
			if err != nil {
				t.Fatal(err)
			}
			credentials := filepath.Join(root, "var/lib/kubelet/kubeconfig")
			before, err := os.ReadFile(credentials)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := p.Restart(t.Context(), log, StartParams{Node: previous, Config: flex}); err != nil {
					t.Fatal(err)
				}
				save, err := p.Start(t.Context(), log, StartParams{Node: previous, Config: flex})
				if err != nil || save {
					t.Fatalf("reapply running SDK node: persist=%t, err=%v", save, err)
				}
				after, err := os.ReadFile(credentials)
				if err != nil || string(after) != string(before) {
					t.Fatal("soft restart/reapply replaced client credentials")
				}
			}
			params := UpgradeParams{StartParams: StartParams{Node: target, Config: flex}, Previous: previous}
			if err := p.Upgrade(t.Context(), log, params); err != nil {
				t.Fatal(err)
			}
			upgraded, err := layout.Current()
			if err != nil || upgraded == oldVersion {
				t.Fatalf("SDK upgrade did not change version: %v", err)
			}
			applied := previous
			if persisted {
				applied = target
			}
			if err := p.Recover(t.Context(), log, StartParams{Node: applied, Config: flex}); err != nil {
				t.Fatal(err)
			}
			current, err := layout.Current()
			if err != nil {
				t.Fatal(err)
			}
			want := oldVersion
			if persisted {
				want = upgraded
			}
			if current != want {
				t.Fatalf("recovered version = %s, want %s", current, want)
			}
			if staged != 1 {
				t.Fatalf("archive staging calls=%d, want one upgrade", staged)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(snapshot), "none-upgrade-journal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("SDK recovery left journal: %v", err)
			}
			if persisted {
				if err := p.Retire(t.Context(), log, params); err != nil {
					t.Fatal(err)
				}
				versions, err := layout.Versions()
				if err != nil || len(versions) != 1 || !strings.Contains(versions[0], "1.35.0") {
					t.Fatalf("SDK prune versions=%v: %v", versions, err)
				}
			}
		})
	}
}
