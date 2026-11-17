package reset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type resetRuntime struct {
	agentruntime.Runtime
	task   phases.Task
	events *[]string
}

func (r resetRuntime) PlanReset(*slog.Logger) (agentruntime.ResetPlan, error) {
	*r.events = append(*r.events, "discover")
	return r, nil
}

func (resetRuntime) HasHostNativeState() bool { return true }

func (r resetRuntime) Stop() phases.Task {
	return hostnode.Func("stop", func(context.Context) error {
		*r.events = append(*r.events, "stop")
		return nil
	})
}

func (r resetRuntime) Finish() phases.Task { return r.task }

func TestResetIgnoresAppliedStateAndRetainsConfigOnFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		state   string
		missing bool
		fail    bool
	}{
		{name: "absent", missing: true},
		{name: "corrupt", state: "not json"},
		{name: "stale", state: `{"isolation":"nspawn","activeMachine":"kube1"}`},
		{name: "cleanup failure", state: "corrupt", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			state := filepath.Join(dir, "daemon-state.json")
			if !tc.missing {
				if err := os.WriteFile(state, []byte(tc.state), 0o600); err != nil {
					t.Fatal(err)
				}
				if tc.name == "stale" {
					sum := sha256.Sum256([]byte(tc.state))
					if err := os.WriteFile(state+".sha256", []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}

			var events []string
			uninstall := hostnode.Func("uninstall", func(context.Context) error {
				events = append(events, "uninstall")
				return nil
			})
			worker := resetRuntime{events: &events, task: hostnode.Func("worker", func(context.Context) error {
				events = append(events, "worker")
				if tc.fail {
					return errors.New("cleanup failed")
				}
				return nil
			})}
			clear := hostnode.Func("clear", func(context.Context) error {
				events = append(events, "clear")
				return os.RemoveAll(dir)
			})
			log := slog.New(slog.DiscardHandler)
			hub := hostnode.Func("hub", func(context.Context) error {
				events = append(events, "hub")
				return nil
			})
			err := runReset(t.Context(), log, uninstall, daemon.ResetWorker(log, worker, hub), clear)
			if (err != nil) != tc.fail {
				t.Fatalf("reset error = %v", err)
			}
			want := []string{"uninstall", "discover", "stop", "hub", "worker", "clear"}
			if tc.fail {
				want = want[:5]
				if contents, err := os.ReadFile(state); err != nil || string(contents) != tc.state {
					t.Fatalf("state not retained: %q, %v", contents, err)
				}
			} else if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration directory remains: %v", err)
			}
			if !slices.Equal(events, want) {
				t.Fatalf("events = %v, want %v", events, want)
			}
		})
	}
}
