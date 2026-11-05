package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestHostRootMarkersAreTheReleasedBinaryLayout ties the markers to the layout
// released versions installed under the legacy root. A marker that does not
// match it would leave those hosts unmigrated, and the new agent would install a
// second layout beside the one systemd runs.
func TestHostRootMarkersAreTheReleasedBinaryLayout(t *testing.T) {
	t.Parallel()

	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)
	markers := HostRootMarkers()

	tests := []struct {
		name   string
		path   string
		marker bool
	}{
		// Install scripts put the plain binary there on fresh hosts too.
		{name: "compatibility link", path: legacy.BinaryPath, marker: false},
		{name: "blue slot", path: legacy.BluePath, marker: true},
		{name: "green slot", path: legacy.GreenPath, marker: true},
		{name: "current link", path: legacy.CurrentPath, marker: true},
		{name: "last-good link", path: legacy.LastGoodPath, marker: true},
		// Not part of the binary layout, and an older reset can leave the
		// library helpers behind.
		{name: "recovery script", path: recoveryScriptPathUnder(hostroot.LegacyPath), marker: false},
		{name: "nspawn lifecycle helper", path: "/usr/local/bin/unbounded-agent-nspawn-lifecycle", marker: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rel, err := filepath.Rel(hostroot.LegacyPath, tt.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := slices.Contains(markers, rel); got != tt.marker {
				t.Fatalf("%s is a marker = %v, want %v", rel, got, tt.marker)
			}
		})
	}
}

// TestInstallHostBinary covers the copy that seeds the layout under the host
// root: made only when no usable binary is there, from the running one, and
// only kept when it runs.
func TestInstallHostBinary(t *testing.T) {
	t.Parallel()

	// layout seeds a blue/green layout from a binary "installed", which leaves the binary under
	// the host root a link to its current slot, as a reset by an earlier release keeps it.
	layout := func(t *testing.T, paths agentUpgradePaths) {
		t.Helper()
		writeExecutable(t, paths.BinaryPath, "installed")
		if err := ensureAgentUpgradeLayout(t.Context(), slog.New(slog.DiscardHandler), paths); err != nil {
			t.Fatalf("ensureAgentUpgradeLayout: %v", err)
		}
	}
	tests := []struct {
		name          string
		unitInstalled bool
		// setup lays out the host root, and returns the binary that runs, or "" for one
		// elsewhere, "running".
		setup       func(t *testing.T, paths agentUpgradePaths) string
		lockHeld    bool
		verifyErr   error
		wantContent string
		wantLayout  bool
		wantErr     string
		wantErrIs   error
	}{
		{name: "fresh host", setup: func(*testing.T, agentUpgradePaths) string { return "" }, wantContent: "running"},

		// The daemon's: with the agent's unit installed, the binary changes only through an
		// activation.
		{
			name:          "a usable binary is kept while the unit is installed",
			unitInstalled: true,
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				writeExecutable(t, paths.BinaryPath, "installed")
				return ""
			},
			wantContent: "installed",
		},
		{
			name:          "a layout is kept while the unit is installed",
			unitInstalled: true,
			setup:         func(t *testing.T, paths agentUpgradePaths) string { layout(t, paths); return "" },
			wantContent:   "installed",
			wantLayout:    true,
		},
		{
			name:          "a dangling link is replaced while the unit is installed",
			unitInstalled: true,
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				if err := os.Symlink(filepath.Join(filepath.Dir(paths.BinaryPath), "missing"), paths.BinaryPath); err != nil {
					t.Fatal(err)
				}
				return ""
			},
			wantContent: "running",
		},

		// start's, before the agent's unit is installed: the binary is the one running.
		{
			name: "start replaces the binary a reset left",
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				writeExecutable(t, paths.BinaryPath, "installed")
				return ""
			},
			wantContent: "running",
		},
		{
			name: "start keeps the binary it runs from",
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				writeExecutable(t, paths.BinaryPath, "installed")
				return paths.BinaryPath
			},
			wantContent: "installed",
		},
		{
			name:        "start replaces a layout an earlier release's reset kept",
			setup:       func(t *testing.T, paths agentUpgradePaths) string { layout(t, paths); return "" },
			wantContent: "running",
		},
		{
			name: "start run from the layout keeps it",
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				layout(t, paths)
				current, err := filepath.EvalSymlinks(paths.CurrentPath)
				if err != nil {
					t.Fatal(err)
				}
				return current
			},
			wantContent: "installed",
			wantLayout:  true,
		},
		{
			// An install script replaced the layout's link with the binary it runs.
			name: "start removes a layout beside the binary it runs from",
			setup: func(t *testing.T, paths agentUpgradePaths) string {
				layout(t, paths)
				if err := os.Remove(paths.BinaryPath); err != nil {
					t.Fatal(err)
				}
				writeExecutable(t, paths.BinaryPath, "from the script")
				return paths.BinaryPath
			},
			wantContent: "from the script",
		},
		{
			name:        "start doesn't run beside an activation",
			setup:       func(t *testing.T, paths agentUpgradePaths) string { layout(t, paths); return "" },
			lockHeld:    true,
			wantContent: "installed",
			wantLayout:  true,
			wantErrIs:   agentbinary.ErrActivationInProgress,
		},
		{
			// As under /opt mounted noexec.
			name:      "a copy that cannot run is removed",
			setup:     func(*testing.T, agentUpgradePaths) string { return "" },
			verifyErr: errors.New("permission denied"),
			wantErr:   "must not be mounted noexec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths := agentUpgradePathsUnder(filepath.Join(dir, "root"))
			if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o755); err != nil {
				t.Fatal(err)
			}
			self := tt.setup(t, paths)
			if self == "" {
				self = filepath.Join(dir, "running")
				writeExecutable(t, self, "running")
			}
			unitPath := filepath.Join(dir, ServiceUnitName)
			if tt.unitInstalled {
				writeExecutable(t, unitPath, "[Service]\n")
			}
			lockPath := filepath.Join(dir, "agent-upgrade.lock")
			if tt.lockHeld {
				lock, err := agentbinary.AcquireHostActivationLock(lockPath)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}

			verified := ""
			err := installHostBinary(t.Context(), slog.New(slog.DiscardHandler), paths, unitPath, lockPath,
				func() error { return nil },
				func() (string, error) { return self, nil },
				func(_ context.Context, path string) error { verified = path; return tt.verifyErr })

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || !errors.Is(err, tt.verifyErr) {
					t.Fatalf("installHostBinary() error = %v, want one containing %q and the cause", err, tt.wantErr)
				}
				if _, err := os.Lstat(paths.BinaryPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a copy that cannot run was left at %s: %v", paths.BinaryPath, err)
				}
				return
			}
			if !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("installHostBinary() error = %v, want %v", err, tt.wantErrIs)
			}

			got, err := os.ReadFile(paths.BinaryPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.wantContent {
				t.Fatalf("%s holds %q, want %q", paths.BinaryPath, got, tt.wantContent)
			}
			copied := tt.wantContent == "running"
			if wantVerified := map[bool]string{true: paths.BinaryPath}[copied]; verified != wantVerified {
				t.Fatalf("verified %q, want %q", verified, wantVerified)
			}
			if copied {
				info, err := os.Lstat(paths.BinaryPath)
				if err != nil {
					t.Fatal(err)
				}
				if !info.Mode().IsRegular() || info.Mode().Perm() != agentUpgradeBinaryMode {
					t.Fatalf("installed binary is %v, want a regular file with mode %v", info.Mode(), os.FileMode(agentUpgradeBinaryMode))
				}
			}
			_, err = os.Lstat(filepath.Dir(paths.CurrentPath))
			if kept := err == nil; kept != tt.wantLayout {
				t.Fatalf("layout kept = %v, want %v", kept, tt.wantLayout)
			}
		})
	}
}

// TestRemoveLegacySeed removes the copy an install script left in
// /usr/local/bin only once nothing the agent runs is under /usr/local, and only
// when it is a regular file. The library reports a missing root, one linked to
// /usr/local, and one partway through a move as not released; in the last the
// daemon may still run from /usr/local.
func TestRemoveLegacySeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		released    bool
		releasedErr error
		seed        string
		// running stands in for runningFrom; nil is start, which does not check.
		running  func(os.FileInfo) (bool, error)
		wantKept bool
		wantErr  bool
	}{
		{name: "installed under the host root, or linked elsewhere", released: true, seed: "file"},
		{name: "not released: absent, linked, or moving", released: false, seed: "file", wantKept: true},
		{name: "the host root cannot be inspected", releasedErr: errors.New("permission denied"), seed: "file", wantKept: true, wantErr: true},
		{name: "a link is not a seed", released: true, seed: "link", wantKept: true},
		{name: "no seed", released: true, seed: ""},
		{
			name:     "the daemon removes a seed it does not run from",
			released: true,
			seed:     "file",
			running:  func(os.FileInfo) (bool, error) { return false, nil },
		},
		{
			// A daemon a release before the blue/green layout started from
			// /usr/local/bin, now running this release.
			name:     "the daemon keeps the seed it runs from",
			released: true,
			seed:     "file",
			running:  func(os.FileInfo) (bool, error) { return true, nil },
			wantKept: true,
		},
		{
			name:     "a seed that may be running is kept",
			released: true,
			seed:     "file",
			running:  func(os.FileInfo) (bool, error) { return false, errors.New("no /proc") },
			wantKept: true,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			seed := filepath.Join(t.TempDir(), "usr", "local", "bin", binaryName)
			if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
				t.Fatal(err)
			}

			switch tt.seed {
			case "file":
				writeExecutable(t, seed, "seed")
			case "link":
				if err := os.Symlink("/bin/true", seed); err != nil {
					t.Fatal(err)
				}
			}

			released := func() (bool, error) { return tt.released, tt.releasedErr }
			err := removeLegacySeed(slog.New(slog.DiscardHandler), released, seed, tt.running)
			if (err != nil) != tt.wantErr {
				t.Fatalf("removeLegacySeed() error = %v, want error %v", err, tt.wantErr)
			}

			_, err = os.Lstat(seed)
			if kept := err == nil; kept != (tt.wantKept && tt.seed != "") {
				t.Fatalf("seed kept = %v, want %v", kept, tt.wantKept)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		})
	}
}

// TestGuardLegacySeed covers what runs before an AgentUpgrade switches the
// binary: the seed goes, and on a host that no longer runs from /usr/local the
// switch is refused while something there would still be taken for a binary by
// a release before the host root.
func TestGuardLegacySeed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		released   bool
		unfinished bool
		// setup puts something at the seed path; dir is a scratch directory.
		setup func(t *testing.T, seed, dir string)
		// running stands in for runningFrom; nil reports nothing running.
		running func(os.FileInfo) (bool, error)
		// servedBySeed has the agent service's main process run the seed,
		// for direct activation, which runs from another path.
		servedBySeed bool
		wantErr      bool
		wantMsg      string
		wantKept     bool
	}{
		{name: "nothing there", released: true, setup: func(*testing.T, string, string) {}},
		{
			name:     "a seed is removed",
			released: true,
			setup:    func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
		},
		{
			name:     "a link to an executable is refused",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				target := filepath.Join(dir, "agent")
				writeExecutable(t, target, "agent")
				if err := os.Symlink(target, seed); err != nil {
					t.Fatal(err)
				}
			},
			wantErr:  true,
			wantMsg:  "/usr/local/sbin",
			wantKept: true,
		},
		{
			// The seed is kept while this process runs from it, so the switch
			// waits for the agent to run from the host root.
			name:     "a seed this process runs from is refused",
			released: true,
			setup:    func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
			running:  func(os.FileInfo) (bool, error) { return true, nil },
			wantErr:  true,
			wantMsg:  "restart " + ServiceUnitName,
			wantKept: true,
		},
		{
			// A host an earlier release installed in /usr/local/bin, updated by
			// rerunning the install: its service runs the seed until it restarts.
			name:         "a seed the agent service runs from is refused to direct activation",
			released:     true,
			setup:        func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
			servedBySeed: true,
			wantErr:      true,
			wantMsg:      "restart " + ServiceUnitName,
			wantKept:     true,
		},
		{
			// An operator's link to the running agent is not the seed.
			name:     "a link to the running binary is refused as a link",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				target := filepath.Join(dir, "agent")
				writeExecutable(t, target, "agent")
				if err := os.Symlink(target, seed); err != nil {
					t.Fatal(err)
				}
			},
			running:  func(os.FileInfo) (bool, error) { return true, nil },
			wantErr:  true,
			wantMsg:  "/usr/local/sbin",
			wantKept: true,
		},
		{
			// An earlier release's compatibility link, once the move removed
			// what it led to.
			name:     "a dangling link is allowed",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				if err := os.Symlink(filepath.Join(dir, "missing"), seed); err != nil {
					t.Fatal(err)
				}
			},
			wantKept: true,
		},
		{
			name:     "a link to a file that cannot run is allowed",
			released: true,
			setup: func(t *testing.T, seed, dir string) {
				target := filepath.Join(dir, "notes")
				if err := os.WriteFile(target, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, seed); err != nil {
					t.Fatal(err)
				}
			},
			wantKept: true,
		},
		{
			// The release before the host root that runs there needs it.
			name:     "a host still running from /usr/local is left alone",
			released: false,
			setup:    func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
			wantKept: true,
		},
		{
			// Its last step failed: the files under /usr/local are still a
			// whole installation, which a release before the host root would
			// run from.
			name:       "a move that has not finished is refused",
			released:   false,
			unfinished: true,
			setup:      func(t *testing.T, seed, _ string) { writeExecutable(t, seed, "seed") },
			wantErr:    true,
			wantMsg:    "restart " + ServiceUnitName,
			wantKept:   true,
		},
		{
			name:       "a move that has not finished is refused with nothing in /usr/local/bin",
			released:   false,
			unfinished: true,
			setup:      func(*testing.T, string, string) {},
			wantErr:    true,
			wantMsg:    "has not finished",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			seed := filepath.Join(dir, "usr", "local", "bin", binaryName)
			if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, seed, dir)

			running := tt.running
			if running == nil {
				running = func(os.FileInfo) (bool, error) { return false, nil }
			}
			if tt.servedBySeed {
				proc := filepath.Join(dir, "proc")
				if err := os.MkdirAll(filepath.Join(proc, "42"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(seed, filepath.Join(proc, "42", "exe")); err != nil {
					t.Fatal(err)
				}
				running = func(info os.FileInfo) (bool, error) {
					return runningOrServingFrom(info, func(os.FileInfo) (bool, error) { return false, nil },
						func() (string, error) { return "42\n", nil }, proc)
				}
			}
			err := guardLegacySeed(slog.New(slog.DiscardHandler),
				func() (bool, error) { return tt.released, nil },
				func() (bool, error) { return tt.unfinished, nil },
				seed, running)
			if (err != nil) != tt.wantErr {
				t.Fatalf("guardLegacySeed() error = %v, want error %v", err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("the refusal does not say what to do (%q): %v", tt.wantMsg, err)
			}

			_, err = os.Lstat(seed)
			if kept := err == nil; kept != tt.wantKept {
				t.Fatalf("seed path kept = %v, want %v", kept, tt.wantKept)
			}
		})
	}

	finished := func() (bool, error) { return false, nil }
	if err := guardLegacySeed(slog.New(slog.DiscardHandler), func() (bool, error) { return false, errors.New("permission denied") },
		finished, filepath.Join(t.TempDir(), binaryName), runningFrom); err == nil {
		t.Fatal("guardLegacySeed() allowed a switch on a host root it could not inspect")
	}
	if err := guardLegacySeed(slog.New(slog.DiscardHandler), func() (bool, error) { return true, nil },
		func() (bool, error) { return false, errors.New("permission denied") },
		filepath.Join(t.TempDir(), binaryName), runningFrom); err == nil {
		t.Fatal("guardLegacySeed() allowed a switch where it could not tell whether a move has finished")
	}
}

// TestRunningOrServingFrom: direct activation counts a file as in use when this
// process runs it, or the agent service's main process does, compared as files
// through /proc/<MainPID>/exe.
func TestRunningOrServingFrom(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	other := filepath.Join(dir, "other")
	writeExecutable(t, seed, "seed")
	writeExecutable(t, other, "other")
	proc := filepath.Join(dir, "proc")
	for pid, exe := range map[string]string{"42": seed, "43": other} {
		if err := os.MkdirAll(filepath.Join(proc, pid), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, filepath.Join(proc, pid, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(seed)
	if err != nil {
		t.Fatal(err)
	}
	notSelf := func(os.FileInfo) (bool, error) { return false, nil }

	for _, tt := range []struct {
		name    string
		self    func(os.FileInfo) (bool, error)
		mainPID func() (string, error)
		want    bool
		wantErr bool
	}{
		{
			name:    "this process runs it",
			self:    func(os.FileInfo) (bool, error) { return true, nil },
			mainPID: func() (string, error) { t.Error("the service was asked"); return "", nil },
			want:    true,
		},
		{name: "the service runs it", self: notSelf, mainPID: func() (string, error) { return "42\n", nil }, want: true},
		{name: "the service runs another file", self: notSelf, mainPID: func() (string, error) { return "43", nil }},
		{name: "the service has no process", self: notSelf, mainPID: func() (string, error) { return "0", nil }},
		{name: "the service's process has ended", self: notSelf, mainPID: func() (string, error) { return "44", nil }},
		{
			name:    "the service can't be asked",
			self:    notSelf,
			mainPID: func() (string, error) { return "", errors.New("systemctl failed") },
			wantErr: true,
		},
		{name: "the service reports no PID", self: notSelf, mainPID: func() (string, error) { return "", nil }, wantErr: true},
		{
			name:    "this process can't tell",
			self:    func(os.FileInfo) (bool, error) { return false, errors.New("permission denied") },
			mainPID: func() (string, error) { return "42", nil },
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := runningOrServingFrom(info, tt.self, tt.mainPID, proc)
			if (err != nil) != tt.wantErr {
				t.Fatalf("runningOrServingFrom() error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("runningOrServingFrom() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunningFrom compares files, not paths: the test binary is found through
// its own path, and a copy of it is not the running file.
func TestRunningFrom(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "copy")
	if err := copyExecutable(self, copied); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		path string
		want bool
	}{
		{name: "the running executable", path: self, want: true},
		{name: "a copy of it", path: copied, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, err := os.Lstat(tt.path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runningFrom(info)
			if err != nil {
				t.Fatalf("runningFrom() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("runningFrom(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestRecoveryAgentUpgradePaths: recovery restores the layout the recovery
// unit ran it from, a blue or green slot, whatever the host root resolves to.
func TestRecoveryAgentUpgradePaths(t *testing.T) {
	t.Parallel()

	moved := agentUpgradePathsUnder(hostroot.Path)
	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)

	tests := []struct {
		name     string
		running  string
		runErr   error
		resolved string
		want     agentUpgradePaths
	}{
		{name: "blue slot under the host root", running: moved.BluePath, resolved: hostroot.LegacyPath, want: moved},
		{name: "green slot under the legacy root", running: legacy.GreenPath, resolved: hostroot.Path, want: legacy},
		{name: "not a slot", running: "/tmp/aks-flex-node", resolved: hostroot.Path, want: moved},
		{name: "a name that only looks like a slot", running: "/srv/lib/aks-flex-node/aks-flex-node-blue-old", resolved: hostroot.Path, want: moved},
		{name: "unknown executable", runErr: errors.New("no /proc"), resolved: hostroot.LegacyPath, want: legacy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := recoveryAgentUpgradePaths(
				func() (string, error) { return tt.running, tt.runErr },
				func() string { return tt.resolved })
			if got != tt.want {
				t.Fatalf("recoveryAgentUpgradePaths() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestVerifyMovedAgent runs the current link of a copied layout, which a move
// does before any unit names the copy.
func TestVerifyMovedAgent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths := agentUpgradePathsUnder(root)
	if err := os.MkdirAll(filepath.Dir(paths.CurrentPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := verifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("verifyMovedAgent() passed a layout with no current binary")
	}

	writeExecutable(t, paths.GreenPath, "#!/bin/sh\n[ \"$1\" = version ]\n")
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := verifyMovedAgent(t.Context(), root); err != nil {
		t.Fatalf("verifyMovedAgent() error = %v", err)
	}

	if err := os.Chmod(paths.GreenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("verifyMovedAgent() passed a binary that cannot run")
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}
