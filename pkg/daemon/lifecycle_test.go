package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

func TestEnsureAgentUpgradeServiceAssetsMigratesExistingInstallation(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.BinaryPath, []byte("legacy"), 0o755); err != nil {
		t.Fatalf("write legacy binary: %v", err)
	}
	systemdDir := filepath.Join(t.TempDir(), "systemd")
	if err := os.MkdirAll(systemdDir, 0o750); err != nil {
		t.Fatalf("MkdirAll systemd: %v", err)
	}
	unitPath := filepath.Join(systemdDir, ServiceUnitName)
	if err := os.WriteFile(unitPath, []byte("[Service]\nExecStart=/usr/local/bin/aks-flex-node agent\n"), 0o644); err != nil {
		t.Fatalf("write legacy unit: %v", err)
	}
	recoveryPath := filepath.Join(t.TempDir(), "aks-flex-node-recovery.sh")
	reloaded := false
	if _, err := ensureAgentUpgradeServiceAssetsAt(
		t.Context(),
		slog.Default(),
		paths,
		agentServiceOptions{},
		systemdDir,
		recoveryPath,
		func(context.Context, *slog.Logger) error {
			reloaded = true
			return nil
		},
		false,
	); err != nil {
		t.Fatalf("ensureAgentUpgradeServiceAssetsAt: %v", err)
	}
	if !reloaded {
		t.Fatal("systemd reload was not requested")
	}
	assertResolvedPath(t, paths.BinaryPath, paths.BluePath)
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
	// For hooks a release before the nspawn lifecycle units writes, after a rollback.
	assertResolvedPath(t, filepath.Join(filepath.Dir(paths.BinaryPath), nspawnLifecycleHelperName), paths.BluePath)
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read service unit: %v", err)
	}
	if !strings.Contains(string(unit), "OnFailure="+recoveryServiceUnitName) {
		t.Fatalf("updated unit does not include recovery: %s", unit)
	}
	if !strings.Contains(string(unit), "ExecStart="+paths.CurrentPath+" agent") {
		t.Fatalf("updated unit does not execute the managed current link: %s", unit)
	}
	recoveryService, err := os.ReadFile(filepath.Join(systemdDir, recoveryServiceUnitName))
	if err != nil {
		t.Fatalf("recovery service was not installed: %v", err)
	}
	if !strings.Contains(string(recoveryService), "ExecStart="+recoveryPath) {
		t.Fatalf("recovery service does not use installed script: %s", recoveryService)
	}
	info, err := os.Stat(recoveryPath)
	if err != nil {
		t.Fatalf("recovery script was not installed: %v", err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf("recovery script mode = %o, want 750", info.Mode().Perm())
	}
}

// While an AgentUpgrade is pending, the daemon may be its candidate, not yet judged, and the
// service assets on disk are the last-good version's, which recovery runs through.
func TestEnsureDaemonServiceAssetsKeepsTheLastGoodAssetsWhileAnUpgradeIsPending(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pending      bool
		missing      []string
		wantHeld     bool
		wantRewrites []string
	}{
		"keeps the last-good version's assets": {
			pending:  true,
			wantHeld: true,
		},
		"writes a missing one": {
			pending:      true,
			missing:      []string{recoveryServiceUnitName},
			wantHeld:     true,
			wantRewrites: []string{recoveryServiceUnitName},
		},
		// An upgrade from a release before them: the candidate's hooks start them, so they're
		// written while the upgrade is pending.
		"writes the nspawn lifecycle units an earlier release didn't have": {
			pending:      true,
			missing:      []string{config.NSpawnPreStartUnit, config.NSpawnPostStartUnit},
			wantHeld:     true,
			wantRewrites: []string{config.NSpawnPreStartUnit, config.NSpawnPostStartUnit},
		},
		"converges them without a pending upgrade": {
			wantRewrites: []string{"recovery script", recoveryServiceUnitName, ServiceUnitName,
				config.NSpawnPreStartUnit, config.NSpawnPostStartUnit},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := os.WriteFile(paths.BinaryPath, []byte("last good"), 0o755); err != nil {
				t.Fatalf("write binary: %v", err)
			}
			systemdDir := filepath.Join(t.TempDir(), "systemd")
			recoveryPath := filepath.Join(t.TempDir(), "aks-flex-node-recovery.sh")
			assets := map[string]string{
				"recovery script":          recoveryPath,
				recoveryServiceUnitName:    filepath.Join(systemdDir, recoveryServiceUnitName),
				ServiceUnitName:            filepath.Join(systemdDir, ServiceUnitName),
				config.NSpawnPreStartUnit:  filepath.Join(systemdDir, config.NSpawnPreStartUnit),
				config.NSpawnPostStartUnit: filepath.Join(systemdDir, config.NSpawnPostStartUnit),
			}
			lastGood := "the last-good version's\n"
			for name, path := range assets {
				if slices.Contains(tc.missing, name) {
					continue
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(path, []byte(lastGood), 0o644); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}
			if tc.pending {
				if err := os.MkdirAll(filepath.Dir(paths.SignalPath), 0o750); err != nil {
					t.Fatalf("MkdirAll: %v", err)
				}
				if err := os.WriteFile(paths.SignalPath, []byte("{}"), 0o600); err != nil {
					t.Fatalf("write signal: %v", err)
				}
			}

			held, err := ensureAgentUpgradeServiceAssetsAt(t.Context(), slog.Default(), paths, agentServiceOptions{},
				systemdDir, recoveryPath, func(context.Context, *slog.Logger) error { return nil }, true)
			if err != nil {
				t.Fatalf("ensureAgentUpgradeServiceAssetsAt: %v", err)
			}
			if held != tc.wantHeld {
				t.Errorf("held = %v, want %v", held, tc.wantHeld)
			}
			for name, path := range assets {
				content, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read %s: %v", name, err)
					continue
				}
				if rewritten := string(content) != lastGood; rewritten != slices.Contains(tc.wantRewrites, name) {
					t.Errorf("%s rewritten = %v, want %v", name, rewritten, !rewritten)
				}
			}
		})
	}
}

// TestNSpawnLifecycleUnits pins the template units the machines' hooks start: each start of a
// machine runs the current agent's phase again, pre-start's hang fails for its caller to retry,
// and post-start is bounded by the machine's own start, which stops it with the machine's.
func TestNSpawnLifecycleUnits(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	systemdDir := filepath.Join(t.TempDir(), "systemd")
	assets, err := desiredAgentServiceAssets(paths, agentServiceOptions{}, systemdDir, filepath.Join(t.TempDir(), "recovery.sh"), paths.CurrentPath)
	if err != nil {
		t.Fatal(err)
	}
	content := map[string]string{}
	agentUnit := slices.IndexFunc(assets, func(a agentServiceAsset) bool { return filepath.Base(a.path) == ServiceUnitName })
	for i, asset := range assets {
		name := filepath.Base(asset.path)
		content[name] = string(asset.content)
		if (name == config.NSpawnPreStartUnit || name == config.NSpawnPostStartUnit) && i > agentUnit {
			t.Errorf("%s is written after %s", name, ServiceUnitName)
		}
	}

	for _, tc := range []struct {
		unit, phase string
		want        []string
	}{
		{unit: config.NSpawnPreStartUnit, phase: "pre-start", want: []string{"TimeoutStartSec=2min"}},
		{unit: config.NSpawnPostStartUnit, phase: "post-start", want: []string{"BindsTo=systemd-nspawn@%i.service", "TimeoutStartSec=infinity"}},
	} {
		unit, ok := content[tc.unit]
		if !ok {
			t.Errorf("%s is not among the service assets", tc.unit)
			continue
		}
		lines := strings.Split(unit, "\n")
		for _, want := range append(tc.want, "Type=oneshot", "ExecStart="+paths.CurrentPath+" nspawn-lifecycle "+tc.phase+" %i") {
			if !slices.Contains(lines, want) {
				t.Errorf("%s has no line %q:\n%s", tc.unit, want, unit)
			}
		}
		for _, line := range lines {
			// A unit that stays active wouldn't run at the machine's next start, and one ordered
			// after the machine would wait for the start that waits for it.
			if strings.HasPrefix(line, "RemainAfterExit=") || strings.HasPrefix(line, "After=") || strings.HasPrefix(line, "[Install]") {
				t.Errorf("%s has %q:\n%s", tc.unit, line, unit)
			}
		}
	}
}

// TestEnsureNSpawnLifecycleHelperLink: the helper beside the agent's binary ends up a link to it,
// whatever was there.
func TestEnsureNSpawnLifecycleHelperLink(t *testing.T) {
	t.Parallel()

	for name, setup := range map[string]func(t *testing.T, helper string){
		"none": func(*testing.T, string) {},
		"a copy an earlier release installed": func(t *testing.T, helper string) {
			if err := os.WriteFile(helper, []byte("an earlier agent"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"a link elsewhere": func(t *testing.T, helper string) {
			if err := os.Symlink("/nonexistent/aks-flex-node", helper); err != nil {
				t.Fatal(err)
			}
		},
		"the link": func(t *testing.T, helper string) {
			if err := os.Symlink(binaryName, helper); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			bin := filepath.Join(t.TempDir(), "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(bin, binaryName)
			writeExecutable(t, binary, "agent")
			helper := filepath.Join(bin, nspawnLifecycleHelperName)
			setup(t, helper)

			if err := ensureNSpawnLifecycleHelperLink(binary); err != nil {
				t.Fatalf("ensureNSpawnLifecycleHelperLink: %v", err)
			}
			if target, err := os.Readlink(helper); err != nil || target != binaryName {
				t.Fatalf("helper links to %q, %v; want %q, relative, so it survives a move of the root", target, err, binaryName)
			}
			if got, err := os.ReadFile(helper); err != nil || string(got) != "agent" {
				t.Fatalf("the helper runs %q, %v; want the agent's binary", got, err)
			}
		})
	}
}

func TestRenderAgentServiceUnitGolden(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		serviceOptions agentServiceOptions
		golden         string
	}{
		"default": {golden: "aks-flex-node-agent.service.golden"},
		"Arc": {
			serviceOptions: agentServiceOptions{ARCEnabled: true},
			golden:         "aks-flex-node-agent-arc.service.golden",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := renderAgentServiceUnit(agentUpgradePathsUnder(hostroot.Path).BinaryPath, tt.serviceOptions)
			if err != nil {
				t.Fatalf("renderAgentServiceUnit() error = %v", err)
			}
			goldenPath := filepath.Join("testdata", tt.golden)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("service unit mismatch (-want +got):\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

func TestInstalledAgentServiceOptions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		writeUnit      bool
		serviceOptions agentServiceOptions
		want           agentServiceOptions
	}{
		"missing unit": {},
		"default unit": {writeUnit: true},
		"Arc unit": {
			writeUnit:      true,
			serviceOptions: agentServiceOptions{ARCEnabled: true},
			want:           agentServiceOptions{ARCEnabled: true},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			systemdDir := t.TempDir()
			if tt.writeUnit {
				unit, err := renderAgentServiceUnit(agentUpgradePathsUnder(hostroot.Path).BinaryPath, tt.serviceOptions)
				if err != nil {
					t.Fatalf("renderAgentServiceUnit() error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(systemdDir, ServiceUnitName), unit, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := installedAgentServiceOptions(systemdDir)
			if err != nil {
				t.Fatalf("installedAgentServiceOptions() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("installedAgentServiceOptions() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestAgentServiceIncludesUpgradeRecovery(t *testing.T) {
	t.Parallel()

	serviceContent, err := renderAgentServiceUnit(agentUpgradePathsUnder(hostroot.Path).BinaryPath, agentServiceOptions{})
	if err != nil {
		t.Fatalf("renderAgentServiceUnit() error = %v", err)
	}
	service := string(serviceContent)
	if !strings.Contains(service, "After=network-online.target systemd-udev-settle.service") {
		t.Fatal("service does not start after systemd-udev-settle.service")
	}
	if !strings.Contains(service, "Wants=network-online.target systemd-udev-settle.service") {
		t.Fatal("service does not activate systemd-udev-settle.service")
	}
	if !strings.Contains(service, "OnFailure="+recoveryServiceUnitName) {
		t.Fatalf("service does not activate %s on failure", recoveryServiceUnitName)
	}
	if !strings.Contains(string(recoveryServiceUnitContent), "ExecStart="+embeddedRecoveryScriptPath) {
		t.Fatalf("recovery service does not execute %s", embeddedRecoveryScriptPath)
	}
	script := string(recoveryScriptContent)
	for _, expected := range []string{
		"recover-agent-upgrade",
		"aks-flex-node-agent.service",
		"aks-flex-node-last-good",
		"agent-upgrade-signal.json",
		"systemctl --no-block restart",
		"|| status=$?",
		"exit \"${status}\"",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("recovery script does not contain %q", expected)
		}
	}
}

// TestRecoveryScriptPathUnder covers the recovery script location. It lives
// beside the blue/green binaries under the host root, and under the legacy root
// it is the path baked into the embedded recovery unit.
func TestRecoveryScriptPathUnder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		root string
		want string
	}{
		{
			name: "legacy root matches the embedded placeholder",
			root: hostroot.LegacyPath,
			want: embeddedRecoveryScriptPath,
		},
		{
			name: "host root",
			root: "/opt/unbounded/agent",
			want: "/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-recovery.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := recoveryScriptPathUnder(tt.root); got != tt.want {
				t.Errorf("recoveryScriptPathUnder(%q) = %q, want %q", tt.root, got, tt.want)
			}
		})
	}
}

// TestRenderRecoveryScriptFollowsTheHostRoot covers the recovery script on a
// host installed under the host root. The script restores the last-good binary
// after a failed upgrade, so if it still names the /usr/local path, rollback
// fails there.
func TestRenderRecoveryScriptFollowsTheHostRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		root     string
		lastGood string
	}{
		{
			name:     "legacy root is unchanged",
			root:     hostroot.LegacyPath,
			lastGood: "/usr/local/lib/aks-flex-node/aks-flex-node-last-good",
		},
		{
			name:     "host root",
			root:     "/opt/unbounded/agent",
			lastGood: "/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-last-good",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := string(renderRecoveryScript(agentUpgradePathsUnder(tt.root)))

			if !strings.Contains(script, "readlink -f "+tt.lastGood) {
				t.Fatalf("recovery script does not read %s:\n%s", tt.lastGood, script)
			}
			if tt.root != hostroot.LegacyPath && strings.Contains(script, "/usr/local/") {
				t.Fatalf("recovery script still names /usr/local:\n%s", script)
			}
		})
	}
}

// TestEmbeddedRecoveryScriptNamesTheLegacyPaths pins the literals
// renderRecoveryScript replaces. If the asset and the legacy layout drift
// apart, the replacement silently matches nothing.
func TestEmbeddedRecoveryScriptNamesTheLegacyPaths(t *testing.T) {
	t.Parallel()

	legacy := agentUpgradePathsUnder(hostroot.LegacyPath)
	script := string(recoveryScriptContent)

	for _, path := range []string{legacy.LastGoodPath, legacy.SignalPath} {
		if !strings.Contains(script, path) {
			t.Fatalf("embedded recovery script does not contain %s", path)
		}
	}
}

// TestUninstallPathsSweepBothRoots covers the files uninstall removes. The
// recovery script is removed under the legacy root too, so uninstalling does
// not depend on the host root having been migrated.
func TestUninstallPathsSweepBothRoots(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		root string
		want []string
	}{
		{
			name: "host root also sweeps the legacy root",
			root: "/opt/unbounded/agent",
			want: []string{
				"/opt/unbounded/agent/lib/aks-flex-node/aks-flex-node-recovery.sh",
				"/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh",
			},
		},
		{
			name: "migrated host sweeps the legacy root once",
			root: hostroot.LegacyPath,
			want: []string{"/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := uninstallPathsUnder(tt.root, hostroot.LegacyPath)
			want := append(tt.want,
				filepath.Join(systemdSystemDir, ServiceUnitName),
				filepath.Join(systemdSystemDir, recoveryServiceUnitName),
				filepath.Join(systemdSystemDir, config.NSpawnPreStartUnit),
				filepath.Join(systemdSystemDir, config.NSpawnPostStartUnit),
				"/etc/aks-flex-node/agent-upgrade-signal.json",
			)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("uninstallPathsUnder(%q) = %v, want %v", tt.root, got, want)
			}
		})
	}
}

// TestRemoveIfPresentSkipsAbsentFiles covers uninstall on a read-only /usr.
// Unlinking a missing file there returns EROFS rather than ENOENT, so the
// remove must not be attempted at all for an absent file.
func TestRemoveIfPresentSkipsAbsentFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		lstat   func(string) (os.FileInfo, error)
		wantErr bool
		removed bool
	}{
		{
			name:    "absent file is not removed",
			lstat:   func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
			wantErr: false,
			removed: false,
		},
		{
			name:    "present file that cannot be removed is an error",
			lstat:   func(string) (os.FileInfo, error) { return nil, nil },
			wantErr: true,
			removed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			removed := false
			err := removeIfPresentWith("/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh", tt.lstat, func(string) error {
				removed = true
				return syscall.EROFS
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("removeIfPresentWith() error = %v, wantErr %v", err, tt.wantErr)
			}
			if removed != tt.removed {
				t.Fatalf("remove called = %v, want %v", removed, tt.removed)
			}
		})
	}
}

// TestRemoveIfPresentRemovesDanglingSymlink pins Lstat over Stat.
func TestRemoveIfPresentRemovesDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "missing"), link); err != nil {
		t.Fatal(err)
	}

	if err := removeIfPresent(link); err != nil {
		t.Fatalf("removeIfPresent() error = %v", err)
	}
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling symlink was not removed: %v", err)
	}
}

func TestRemoveFirstBootUnit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		setup        func(t *testing.T, unitPath string)
		systemctlErr error
		wantErr      bool
		wantCalls    []string
		wantRemoved  bool
	}{
		{
			name:        "absent unit is left alone",
			setup:       func(*testing.T, string) {},
			wantCalls:   nil,
			wantRemoved: true,
		},
		{
			name: "installed unit is disabled, stopped, and removed",
			setup: func(t *testing.T, unitPath string) {
				if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCalls:   []string{"disable --now " + FirstBootUnitName},
			wantRemoved: true,
		},
		{
			name: "dangling unit link is still disabled and removed",
			setup: func(t *testing.T, unitPath string) {
				if err := os.Symlink(unitPath+".missing", unitPath); err != nil {
					t.Fatal(err)
				}
			},
			wantCalls:   []string{"disable --now " + FirstBootUnitName},
			wantRemoved: true,
		},
		{
			name: "unit that cannot be disabled is kept and reported",
			setup: func(t *testing.T, unitPath string) {
				if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			systemctlErr: errors.New("systemctl failed"),
			wantErr:      true,
			wantCalls:    []string{"disable --now " + FirstBootUnitName},
			wantRemoved:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			unitPath := filepath.Join(dir, FirstBootUnitName)
			tt.setup(t, unitPath)

			var calls []string
			err := removeFirstBootUnit(t.Context(), discardLogger(), dir, func(_ context.Context, args ...string) error {
				calls = append(calls, strings.Join(args, " "))
				return tt.systemctlErr
			})

			if (err != nil) != tt.wantErr {
				t.Fatalf("removeFirstBootUnit() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Fatalf("systemctl calls = %q, want %q", calls, tt.wantCalls)
			}
			_, statErr := os.Lstat(unitPath)
			if removed := errors.Is(statErr, os.ErrNotExist); removed != tt.wantRemoved {
				t.Fatalf("unit removed = %v, want %v", removed, tt.wantRemoved)
			}
		})
	}
}

func TestUninstallServiceStopsTheUnitLast(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		stopErr       error
		stopEndsTask  bool
		busyPath      bool
		firstBootUnit bool
		wantCalls     []string
		wantErr       bool
	}{
		"disables, removes, and reloads before stopping": {
			wantCalls: []string{"disable " + ServiceUnitName, "daemon-reload", "stop " + ServiceUnitName},
		},
		// On a host provisioned by Ignition. The first-boot unit goes first, while the agent,
		// which may be the process running this task, is still running.
		"removes the first-boot unit before the agent's": {
			firstBootUnit: true,
			wantCalls: []string{
				"disable --now " + FirstBootUnitName,
				"disable " + ServiceUnitName,
				"daemon-reload",
				"stop " + ServiceUnitName,
			},
		},
		"a failed stop is a warning": {
			stopErr:   errors.New("unit not loaded"),
			wantCalls: []string{"disable " + ServiceUnitName, "daemon-reload", "stop " + ServiceUnitName},
		},
		// As when the daemon uninstalls its own unit: the stop's SIGTERM cancels its context,
		// with everything else already done.
		"a stop that ends the caller leaves nothing undone": {
			stopErr:      context.Canceled,
			stopEndsTask: true,
			wantCalls:    []string{"disable " + ServiceUnitName, "daemon-reload", "stop " + ServiceUnitName},
		},
		"stops nothing when it can't remove a file": {
			busyPath:  true,
			wantCalls: []string{"disable " + ServiceUnitName},
			wantErr:   true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths := []string{
				filepath.Join(dir, ServiceUnitName),
				filepath.Join(dir, recoveryServiceUnitName),
				filepath.Join(dir, "recovery.sh"),
				filepath.Join(dir, "missing-signal.json"),
			}
			for _, path := range paths[:3] {
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.busyPath {
				// A non-empty directory where a file belongs can't be removed.
				if err := os.MkdirAll(filepath.Join(paths[3], "child"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			firstBootUnit := filepath.Join(dir, FirstBootUnitName)
			if tc.firstBootUnit {
				if err := os.WriteFile(firstBootUnit, []byte("[Unit]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls []string
			task := &uninstallServiceTask{
				log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
				unitDir: dir,
				paths:   paths,
				systemctl: func(_ context.Context, args ...string) error {
					calls = append(calls, strings.Join(args, " "))
					switch args[0] {
					case "daemon-reload":
						for _, path := range append([]string{firstBootUnit}, paths...) {
							if _, err := os.Stat(path); err == nil {
								t.Errorf("%s is still there when systemd reloads", path)
							}
						}
					case "stop":
						if tc.stopEndsTask {
							cancel()
						}
						return tc.stopErr
					}
					return nil
				},
			}

			err := task.Do(ctx)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Do() error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(calls, tc.wantCalls) {
				t.Errorf("systemctl calls = %q, want %q", calls, tc.wantCalls)
			}
		})
	}
}

// TestFirstBootUnitIsConditionedOnTheAgentUnit pins the path the first-boot unit checks, which
// has to be where the agent unit is actually written.
func TestFirstBootUnitIsConditionedOnTheAgentUnit(t *testing.T) {
	t.Parallel()

	if want := filepath.Join(systemdSystemDir, ServiceUnitName); ServiceUnitPath != want {
		t.Fatalf("ServiceUnitPath = %q, want %q", ServiceUnitPath, want)
	}
}
