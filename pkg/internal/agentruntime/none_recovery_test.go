package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostnode/hostnodetest"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestNoneRecoveryUsesPersistedConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		persisted bool
	}{
		{name: "save failed"},
		{name: "saved before commit", persisted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, units, proc := t.TempDir(), t.TempDir(), t.TempDir()
			layout := none.Layout{Base: filepath.Join(t.TempDir(), "node")}
			snapshot := filepath.Join(t.TempDir(), "snapshot")
			active := map[string]bool{}
			stops := 0
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
					stops++
					active[unit] = false
				case "enable", "restart":
					active[unit] = true
					if unit == none.UnitKubelet {
						credential := filepath.Join(root, "var/lib/kubelet/kubeconfig")
						if _, err := os.Stat(credential); errors.Is(err, os.ErrNotExist) {
							if err := os.MkdirAll(filepath.Dir(credential), 0o700); err != nil {
								return "", err
							}
							if err := os.WriteFile(credential, []byte("client identity"), 0o600); err != nil {
								return "", err
							}
						}
					}
				}
				return "", nil
			}
			goal := func() *goalstates.MachineGoalState {
				gs := hostnodetest.GoalState("kube1", false)
				gs.RootFS.KubernetesVersion = "1.35.0"
				return gs
			}
			runtime := noneRuntime{newNode: func(params noneops.Params) *noneops.Node {
				params.GoalState = goal()
				if params.Previous != nil {
					params.PreviousGoalState = goal()
				}
				params.Overrides = noneops.Overrides{
					Layout:      layout,
					SystemdDir:  units,
					Proc:        proc,
					Run:         run,
					Download:    func(context.Context) error { return nil },
					WaitKubelet: func(context.Context) error { return nil },
				}
				params.Hooks = hostnode.Hooks{
					HostRoot:          root,
					ConfigSnapshotDir: snapshot,
					SetupNetwork:      func(context.Context) error { return nil },
					CleanupNetwork:    func(context.Context) error { return nil },
				}
				return noneops.New(params)
			}}
			log := slog.New(slog.DiscardHandler)
			previous := &agentconfig.AgentConfig{
				Isolation: "none",
				Cluster:   agentconfig.AgentClusterConfig{Version: "1.35.0"},
				NodeName:  "old",
			}
			target := previous.DeepCopy()
			target.NodeName = "new"
			initial := runtime.newNode(noneops.Params{Log: log, Config: previous})
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
			upgrade := runtime.newNode(noneops.Params{Log: log, Config: target, Previous: previous})
			if err := upgrade.Upgrade(t.Context()); err != nil {
				t.Fatal(err)
			}
			applied := previous
			if tc.persisted {
				applied = target
			}
			beforeRecovery := stops
			cfg := &config.Config{
				Bootstrap: config.BootstrapConfig{
					OfflineArtifacts: config.OfflineArtifactsConfig{Source: "offline"},
				},
			}
			input := Inputs{
				Config: cfg,
				Node:   Node{Slot: "kube1", Config: applied},
			}
			if err := runtime.Prepare(log, input).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			if rolledBack := stops > beforeRecovery; rolledBack == tc.persisted {
				t.Fatalf("rolled back = %t, persisted = %t", rolledBack, tc.persisted)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(snapshot), "none-upgrade-journal.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains: %v", err)
			}
			if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("snapshot remains: %v", err)
			}
		})
	}
}
