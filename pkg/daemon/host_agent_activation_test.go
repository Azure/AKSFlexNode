package daemon

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestFlexDaemonActivationPreflightUsesFlexAssetsWithoutMutation(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	systemdDir := filepath.Join(t.TempDir(), "systemd")
	recoveryScript := filepath.Join(t.TempDir(), "recovery.sh")
	service := &flexDaemonActivationService{
		log:            slog.Default(),
		paths:          paths,
		systemdDir:     systemdDir,
		recoveryScript: recoveryScript,
	}
	plan, err := service.Preflight(t.Context(), paths.CurrentPath)
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !plan.UpdateRequired {
		t.Fatal("UpdateRequired = false for missing Flex service assets")
	}
	for _, path := range []string{systemdDir, recoveryScript, paths.CurrentPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight mutated %s: %v", path, err)
		}
	}
}

func TestFlexDaemonActivationWithoutAppliedStateSkipsNspawnSynchronization(t *testing.T) {
	t.Parallel()

	service := &flexDaemonActivationService{
		log:   slog.Default(),
		state: &testStateStore{},
	}
	if err := service.synchronizeActiveNspawn(t.Context(), "/unused/host/binary"); err != nil {
		t.Fatalf("synchronizeActiveNspawn: %v", err)
	}
}

func TestFlexDaemonActivationPreflightRejectsMachineOperationSignal(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.SignalPath), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(paths.SignalPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	service := &flexDaemonActivationService{
		log:            slog.Default(),
		paths:          paths,
		systemdDir:     t.TempDir(),
		recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
	}
	if _, err := service.Preflight(t.Context(), paths.CurrentPath); err == nil {
		t.Fatal("Preflight accepted a pending MachineOperation signal")
	}
}
