package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

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
	if err := ensureAgentUpgradeServiceAssetsAt(
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
	); err != nil {
		t.Fatalf("ensureAgentUpgradeServiceAssetsAt: %v", err)
	}
	if !reloaded {
		t.Fatal("systemd reload was not requested")
	}
	assertResolvedPath(t, paths.BinaryPath, paths.BluePath)
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
	assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
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
			root: "/opt/unbounded",
			want: "/opt/unbounded/lib/aks-flex-node/aks-flex-node-recovery.sh",
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
			root:     "/opt/unbounded",
			lastGood: "/opt/unbounded/lib/aks-flex-node/aks-flex-node-last-good",
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
			root: "/opt/unbounded",
			want: []string{
				"/opt/unbounded/lib/aks-flex-node/aks-flex-node-recovery.sh",
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
			err := removeFirstBootUnit(t.Context(), discardLogger(), dir, func(_ context.Context, _ *slog.Logger, args ...string) error {
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

// TestFirstBootUnitIsConditionedOnTheAgentUnit pins the path the first-boot unit checks, which
// has to be where the agent unit is actually written.
func TestFirstBootUnitIsConditionedOnTheAgentUnit(t *testing.T) {
	t.Parallel()

	if want := filepath.Join(systemdSystemDir, ServiceUnitName); ServiceUnitPath != want {
		t.Fatalf("ServiceUnitPath = %q, want %q", ServiceUnitPath, want)
	}
}
