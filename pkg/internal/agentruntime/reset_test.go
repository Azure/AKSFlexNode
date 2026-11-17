package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

type testResetWorker struct {
	name       string
	events     *[]string
	present    bool
	discovered int
	fail       string
}

func (w *testResetWorker) Present() bool {
	w.discovered++
	return w.present
}

func (w *testResetWorker) task(stage string) phases.Task {
	return hostnode.Func(stage, func(context.Context) error {
		*w.events = append(*w.events, w.name+"-"+stage)
		if stage == "stop" {
			w.present = false
		}
		if stage == w.fail {
			return errors.New("failed " + stage)
		}
		return nil
	})
}

func (w *testResetWorker) Stop() phases.Task    { return w.task("stop") }
func (w *testResetWorker) Destroy() phases.Task { return w.task("destroy") }

func TestResetPlanDiscoveryAndOrdering(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		present bool
		fail    string
		want    []string
	}{
		{
			name:    "both workers",
			present: true,
			want:    []string{"nspawn-stop", "none-stop", "nspawn-destroy", "none-destroy", "shared"},
		},
		{
			name: "native absent",
			want: []string{"nspawn-stop", "nspawn-destroy", "orphan-artifacts", "shared"},
		},
		{
			name:    "native stop fails",
			present: true,
			fail:    "stop",
			want:    []string{"nspawn-stop", "none-stop"},
		},
		{
			name:    "native destroy fails",
			present: true,
			fail:    "destroy",
			want:    []string{"nspawn-stop", "none-stop", "nspawn-destroy", "none-destroy"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var events []string
			machine := &testResetWorker{name: "nspawn", events: &events}
			native := &testResetWorker{name: "none", events: &events, present: tc.present, fail: tc.fail}
			record := func(name string) phases.Task {
				return hostnode.Func(name, func(context.Context) error {
					events = append(events, name)
					return nil
				})
			}
			log := slog.New(slog.DiscardHandler)
			plan := discoverReset(log, machine, native, record("shared"), record("orphan-artifacts"), false)
			if (plan.none != nil) != tc.present {
				t.Fatal("incorrect discovery")
			}
			err := phases.Serial(log, plan.Stop(), plan.Finish()).Do(t.Context())
			if (err != nil) != (tc.fail != "") {
				t.Fatalf("reset error = %v", err)
			}
			if native.discovered != 1 {
				t.Fatalf("discovery calls = %d", native.discovered)
			}
			if !slices.Equal(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
		})
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

	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "missing-target"), dangling); err != nil {
		t.Fatal(err)
	}

	task := &removeFilesTask{
		name:  "test",
		log:   slog.New(slog.DiscardHandler),
		paths: []string{present, dangling, filepath.Join(dir, "absent")},
	}

	for pass := 1; pass <= 2; pass++ {
		if err := task.Do(t.Context()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	for _, path := range []string{present, dangling} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("file %s was not removed: %v", path, err)
		}
	}
}
