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
		name string
		// root is where the layout was built, and now where the root
		// resolves under the lock.
		root, now     string
		unfinished    bool
		unfinishedErr error
		wantMsg       string // empty: no error
	}{
		{name: "unchanged root", root: hostroot.LegacyPath, now: hostroot.LegacyPath},
		{name: "root moved under the activation", root: hostroot.LegacyPath, now: hostroot.Path, wantMsg: "retry"},
		// A move's first pass renamed its copy into place before the layout
		// was built: the root is unchanged, but the files under the legacy
		// root are still there.
		{name: "unchanged root, move not finished", root: hostroot.Path, now: hostroot.Path, unfinished: true, wantMsg: "restart " + ServiceUnitName},
		{name: "unchanged root, move state unknown", root: hostroot.Path, now: hostroot.Path, unfinishedErr: errors.New("permission denied"), wantMsg: "inspect the host root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			paths := testAgentUpgradePaths(t)
			inspected := false
			service := &flexDaemonActivationService{
				log:            slog.Default(),
				root:           tt.root,
				resolveRoot:    func() string { return tt.now },
				moveUnfinished: func() (bool, error) { return tt.unfinished, tt.unfinishedErr },
				paths:          paths,
				systemdDir:     t.TempDir(),
				recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
				inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
					inspected = true
					return false, nil
				},
			}

			_, err := service.Preflight(t.Context(), paths.CurrentPath)
			if (err != nil) != (tt.wantMsg != "") {
				t.Fatalf("Preflight() error = %v, want error %v", err, tt.wantMsg != "")
			}
			if err != nil && (inspected || !strings.Contains(err.Error(), tt.wantMsg)) {
				t.Fatalf("Preflight() went on after refusing, or did not say %q: inspected = %v, error = %v", tt.wantMsg, inspected, err)
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

// TestDirectActivationIsRefusedInAContainer: in the agent's container the host has no binary
// layout to switch, and an AgentUpgrade switches images instead. It sets the environment, so it
// doesn't run in parallel.
func TestDirectActivationIsRefusedInAContainer(t *testing.T) {
	t.Setenv(ContainerizedEnv, "true")
	log := slog.New(slog.DiscardHandler)

	if _, err := PreflightHostAgentActivation(t.Context(), log, "/nonexistent/aks-flex-node"); !errors.Is(err, errDirectActivationInContainer) {
		t.Fatalf("PreflightHostAgentActivation() error = %v, want %v", err, errDirectActivationInContainer)
	}
	if _, err := ActivateHostAgent(t.Context(), log, "/nonexistent/aks-flex-node"); !errors.Is(err, errDirectActivationInContainer) {
		t.Fatalf("ActivateHostAgent() error = %v, want %v", err, errDirectActivationInContainer)
	}
}
