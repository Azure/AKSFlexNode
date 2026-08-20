package daemon

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
