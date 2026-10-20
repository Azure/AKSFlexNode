package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
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
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
			return false, nil
		},
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

func TestFlexDaemonActivationLeavesInactiveResetHostStopped(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(binary, []byte("candidate"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	service := &flexDaemonActivationService{
		log:   slog.Default(),
		state: &testStateStore{},
	}
	if err := service.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := service.WaitHealthy(t.Context(), binary); err != nil {
		t.Fatalf("WaitHealthy: %v", err)
	}
}

func TestFlexDaemonActivationPreflightRejectsUnknownServiceState(t *testing.T) {
	t.Parallel()

	service := &flexDaemonActivationService{
		log: slog.Default(),
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
			return false, errors.New("systemd unavailable")
		},
	}
	if _, err := service.Preflight(t.Context(), "/unused/current"); err == nil {
		t.Fatal("Preflight accepted an unknown service state")
	}
}

// TestFlexDaemonActivationPreflightRejectsAMovedHostRoot covers an activation
// whose layout was built before a daemon moved the host to the host root.
// Preflight runs under the activation lock, so it is the first point that sees
// the root as the activation will switch it.
func TestFlexDaemonActivationPreflightRejectsAMovedHostRoot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		now     string
		wantErr bool
	}{
		{name: "unchanged root", now: hostroot.LegacyPath},
		{name: "root moved under the activation", now: hostroot.Path, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			inspected := false
			service := &flexDaemonActivationService{
				log:            slog.Default(),
				root:           hostroot.LegacyPath,
				resolveRoot:    func() string { return tt.now },
				paths:          paths,
				systemdDir:     t.TempDir(),
				recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
				inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
					inspected = true
					return false, nil
				},
			}

			_, err := service.Preflight(t.Context(), paths.CurrentPath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Preflight() error = %v, want error %v", err, tt.wantErr)
			}
			if tt.wantErr && (inspected || !strings.Contains(err.Error(), "retry")) {
				t.Fatalf("Preflight() went on after the root moved, or did not say to retry: inspected = %v, error = %v", inspected, err)
			}
		})
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
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
			return false, nil
		},
	}
	if _, err := service.Preflight(t.Context(), paths.CurrentPath); err == nil {
		t.Fatal("Preflight accepted a pending MachineOperation signal")
	}
}
