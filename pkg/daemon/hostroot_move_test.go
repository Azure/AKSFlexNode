package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestHostLayout ties the files a move copies to every path the daemon and the
// agent library build under the host root.
func TestHostLayout(t *testing.T) {
	t.Parallel()

	paths := agentdeployment.UpgradePathsUnder(hostroot.Path)
	library := goalstates.LegacyHostPaths()
	want := []string{
		paths.BinaryPath, paths.BluePath, paths.GreenPath, paths.CurrentPath, paths.LastGoodPath,
		agentdeployment.RecoveryScriptPathUnder(hostroot.Path),
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

	for _, marker := range agentdeployment.HostRootMarkers() {
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

// The gate stays closed until a moved or restarted agent is safely replaced.
func TestHoldForHostRoot(t *testing.T) {
	t.Parallel()

	legacy := agentdeployment.UpgradePathsUnder(hostroot.LegacyPath)
	root := agentdeployment.UpgradePathsUnder(hostroot.Path)

	tests := []struct {
		name             string
		restarted        bool
		unfinished       bool
		startup, current agentdeployment.UpgradePaths
		wantHold         bool
	}{
		{name: "nothing moved", startup: legacy, current: legacy},
		{name: "installed under the host root", startup: root, current: root},
		// The first pass of a move: the restarted daemon takes the work.
		{name: "restart queued", restarted: true, startup: legacy, current: root, wantHold: true},
		// The move swapped the copy in and then failed: the executor still
		// names the legacy slots the units may no longer run.
		{name: "moved under the running daemon", startup: legacy, current: root, wantHold: true},
		// The second pass of a move failed before it removed the files under
		// the legacy root: the next start retries it, and until then no
		// AgentUpgrade may switch to a release that would run from them.
		{name: "a move that has not finished", unfinished: true, startup: root, current: root, wantHold: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			reason := holdForHostRoot(tt.restarted, tt.unfinished, tt.startup, tt.current)
			if (reason != "") != tt.wantHold {
				t.Fatalf("holdForHostRoot() = %q, want hold %v", reason, tt.wantHold)
			}
		})
	}
}

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

	unit, err := os.ReadFile(filepath.Join("..", "internal", "agentdeployment", "assets", "aks-flex-node-agent.service"))
	if err != nil {
		t.Fatalf("read embedded agent unit: %v", err)
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
