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
			got, err := renderAgentServiceUnit(defaultAgentUpgradePaths().BinaryPath, tt.serviceOptions)
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
				unit, err := renderAgentServiceUnit(defaultAgentUpgradePaths().BinaryPath, tt.serviceOptions)
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

	serviceContent, err := renderAgentServiceUnit(defaultAgentUpgradePaths().BinaryPath, agentServiceOptions{})
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
	if !strings.Contains(string(recoveryServiceUnitContent), "ExecStart="+recoveryScriptPath) {
		t.Fatalf("recovery service does not execute %s", recoveryScriptPath)
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

// TestRecoveryScriptPathForPrefix covers the recovery script location.
//
// It lives beside the blue/green binaries under the host prefix, so on a host
// with a read-only /usr it must move with them. The default reproduces the
// historical path that is baked into the embedded recovery unit.
func TestRecoveryScriptPathForPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{
			name:   "default matches the embedded placeholder",
			prefix: "",
			want:   recoveryScriptPath,
		},
		{
			name:   "custom prefix relocates the script",
			prefix: "/opt/aks-flex-node",
			want:   "/opt/aks-flex-node/lib/aks-flex-node/aks-flex-node-recovery.sh",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := recoveryScriptPathForPrefix(tt.prefix); got != tt.want {
				t.Errorf("recoveryScriptPathForPrefix(%q) = %q, want %q", tt.prefix, got, tt.want)
			}
		})
	}
}

// TestRenderRecoveryScriptFollowsThePrefix covers the recovery script on a host
// with a custom prefix. The script restores the last-good binary after a failed
// upgrade, so if it still names the /usr/local path, rollback fails on exactly
// the hosts that set a prefix.
func TestRenderRecoveryScriptFollowsThePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		prefix   string
		lastGood string
	}{
		{
			name:     "default prefix is unchanged",
			prefix:   "",
			lastGood: "/usr/local/lib/aks-flex-node/aks-flex-node-last-good",
		},
		{
			name:     "custom prefix",
			prefix:   "/opt/aks-flex-node",
			lastGood: "/opt/aks-flex-node/lib/aks-flex-node/aks-flex-node-last-good",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := string(renderRecoveryScript(agentUpgradePathsForPrefix(tt.prefix)))

			if !strings.Contains(script, "readlink -f "+tt.lastGood) {
				t.Fatalf("recovery script does not read %s:\n%s", tt.lastGood, script)
			}
			if tt.prefix != "" && strings.Contains(script, "/usr/local/") {
				t.Fatalf("recovery script still names /usr/local:\n%s", script)
			}
		})
	}
}

// TestEmbeddedRecoveryScriptNamesTheDefaultPaths pins the literals
// renderRecoveryScript replaces. If the asset and the default layout drift
// apart, the replacement silently matches nothing.
func TestEmbeddedRecoveryScriptNamesTheDefaultPaths(t *testing.T) {
	t.Parallel()

	defaults := agentUpgradePathsForPrefix("")
	script := string(recoveryScriptContent)

	for _, path := range []string{defaults.LastGoodPath, defaults.SignalPath} {
		if !strings.Contains(script, path) {
			t.Fatalf("embedded recovery script does not contain %s", path)
		}
	}
}

// TestUninstallPathsSweepEveryPrefix covers the files uninstall removes. The
// recovery script lives under the prefix, and a host that changed prefix still
// has one under the old location, so both are removed.
func TestUninstallPathsSweepEveryPrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		want   []string
	}{
		{
			name:   "default prefix",
			prefix: "",
			want:   []string{"/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh"},
		},
		{
			name:   "custom prefix also sweeps the default",
			prefix: "/opt/aks-flex-node",
			want: []string{
				"/opt/aks-flex-node/lib/aks-flex-node/aks-flex-node-recovery.sh",
				"/usr/local/lib/aks-flex-node/aks-flex-node-recovery.sh",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := uninstallPaths(tt.prefix)
			for _, path := range append(tt.want,
				filepath.Join(systemdSystemDir, ServiceUnitName),
				filepath.Join(systemdSystemDir, recoveryServiceUnitName),
				"/etc/aks-flex-node/agent-upgrade-signal.json",
			) {
				if !slices.Contains(got, path) {
					t.Fatalf("uninstallPaths(%q) = %v, missing %s", tt.prefix, got, path)
				}
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
