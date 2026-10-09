package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestHostLayout ties the files a move copies to every path the daemon and the
// agent library build under the host root.
func TestHostLayout(t *testing.T) {
	t.Parallel()

	paths := agentUpgradePathsUnder(hostroot.Path)
	library := goalstates.LegacyHostPaths()
	want := []string{
		paths.BinaryPath, paths.BluePath, paths.GreenPath, paths.CurrentPath, paths.LastGoodPath,
		recoveryScriptPathUnder(hostroot.Path),
		filepath.Join(hostroot.Path, strings.TrimPrefix(library.NSpawnLifecycleBinary, hostroot.LegacyPath)),
		filepath.Join(hostroot.Path, strings.TrimPrefix(library.LocalDNSNetworkHelper, hostroot.LegacyPath)),
	}

	var got []string
	for _, rel := range hostLayout() {
		got = append(got, filepath.Join(hostroot.Path, rel))
	}
	if !slices.Equal(got, want) {
		t.Fatalf("hostLayout() = %v, want %v", got, want)
	}

	for _, marker := range HostRootMarkers() {
		if !slices.Contains(hostLayout(), marker) {
			t.Fatalf("marker %s is not part of the layout a move copies", marker)
		}
	}

	// A directory the move removes holds AKS Flex Node's own files and none
	// of the agent library's, which install into shared directories.
	for _, dir := range hostLayoutDirs() {
		owned := false
		for _, rel := range hostLayout() {
			owned = owned || filepath.Dir(rel) == dir
		}
		if !owned {
			t.Fatalf("directory %s holds no file of the layout", dir)
		}
		for _, path := range []string{library.NSpawnLifecycleBinary, library.LocalDNSNetworkHelper} {
			if filepath.Dir(path) == filepath.Join(hostroot.LegacyPath, dir) {
				t.Fatalf("directory %s holds the agent library's %s", dir, path)
			}
		}
	}
}

// TestNspawnLifecycleTasksInstallTheHelperFirst: the move copies the helper an
// earlier release installed, which regenerates the machine's units with a path
// under /usr/local that the move removes. Rewriting the units has to replace it
// with this release's helper before the units name it, or the machine does not
// start again after its next restart.
func TestNspawnLifecycleTasksInstallTheHelperFirst(t *testing.T) {
	t.Parallel()

	var names []string
	for _, task := range nspawnLifecycleTasks(slog.New(slog.DiscardHandler), &goalstates.RootFS{}) {
		names = append(names, task.Name())
	}
	if want := []string{"ensure-nspawn-lifecycle-helper", "ensure-nspawn-config"}; !slices.Equal(names, want) {
		t.Fatalf("nspawnLifecycleTasks() = %v, want %v", names, want)
	}
}

func TestSyncDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name    string
		dirs    []string
		wantErr string
	}{
		{name: "nothing to sync"},
		{name: "existing directories, repeated", dirs: []string{dir, dir, t.TempDir()}},
		{name: "a missing directory is reported", dirs: []string{dir, missing}, wantErr: missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := syncDirs(tt.dirs...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("syncDirs() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("syncDirs() error = %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}

// TestHoldForHostRoot keeps the reconcile gate closed whenever the daemon must
// not take work after reconciling the host root.
func TestHoldForHostRoot(t *testing.T) {
	t.Parallel()

	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)
	root := agentUpgradePathsUnder(hostroot.Path)

	tests := []struct {
		name             string
		restarted        bool
		startup, current agentUpgradePaths
		wantHold         bool
	}{
		{name: "nothing moved", startup: legacy, current: legacy},
		{name: "installed under the host root", startup: root, current: root},
		// The first pass of a move: the restarted daemon takes the work.
		{name: "restart queued", restarted: true, startup: legacy, current: root, wantHold: true},
		// The move swapped the copy in and then failed: the executor still
		// names the legacy slots the units may no longer run.
		{name: "moved under the running daemon", startup: legacy, current: root, wantHold: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason := holdForHostRoot(tt.restarted, tt.startup, tt.current)
			if (reason != "") != tt.wantHold {
				t.Fatalf("holdForHostRoot() = %q, want hold %v", reason, tt.wantHold)
			}
		})
	}
}

// TestAwaitReplacement: a daemon that may not take work is idle until systemd
// stops it, and exits with an error if that does not come, so Restart= starts
// it from the units as they are.
func TestAwaitReplacement(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)

	t.Run("stopped by systemd", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := awaitReplacement(ctx, log, "restart queued", time.Hour); err != nil {
			t.Fatalf("awaitReplacement() error = %v, want nil once stopped", err)
		}
	})

	t.Run("not restarted in time", func(t *testing.T) {
		t.Parallel()

		err := awaitReplacement(t.Context(), log, "restart queued", time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "restart queued") {
			t.Fatalf("awaitReplacement() error = %v, want one giving the reason", err)
		}
		if errors.Is(err, context.Canceled) {
			t.Fatal("the error must not read as a cancellation, which the daemon exits 0 on")
		}
	})
}

// TestHostRootRestartWaitStaysClearOfTheStartLimit: a daemon that exits after
// the wait and is restarted RestartSec later must not be able to hit the
// agent unit's start limit, which would trip recovery into a rollback.
func TestHostRootRestartWaitStaysClearOfTheStartLimit(t *testing.T) {
	t.Parallel()

	unit := renderedAgentUnitForTest(t)
	interval := unitDuration(t, unit, "StartLimitIntervalSec")
	burst := unitInt(t, unit, "StartLimitBurst")
	restartDelay := unitDuration(t, unit, "RestartSec")

	if spacing := hostRootRestartWait + restartDelay; time.Duration(burst-1)*spacing <= interval {
		t.Fatalf("%d starts %s apart fit in StartLimitIntervalSec=%s", burst, spacing, interval)
	}
}

func renderedAgentUnitForTest(t *testing.T) string {
	t.Helper()

	unit, err := renderAgentServiceUnit(agentUpgradePathsUnder(hostroot.Path).BinaryPath, agentServiceOptions{})
	if err != nil {
		t.Fatalf("renderAgentServiceUnit() error = %v", err)
	}

	return string(unit)
}

// unitValue returns the value of the first key=value line for key in unit.
func unitValue(t *testing.T, unit, key string) string {
	t.Helper()

	for line := range strings.SplitSeq(unit, "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("the agent unit has no %s", key)

	return ""
}

// unitDuration reads a unit time span given in plain seconds, as the agent
// unit writes them.
func unitDuration(t *testing.T, unit, key string) time.Duration {
	t.Helper()

	return time.Duration(unitInt(t, unit, key)) * time.Second
}

func unitInt(t *testing.T, unit, key string) int {
	t.Helper()

	value := unitValue(t, unit, key)
	n, err := strconv.Atoi(value)
	if err != nil {
		t.Fatalf("%s=%s is not a plain number: %v", key, value, err)
	}

	return n
}
