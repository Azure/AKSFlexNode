package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestFlexDaemonActivationPreflightUsesFlexAssetsWithoutMutation(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	systemdDir := filepath.Join(t.TempDir(), "systemd")
	recoveryScript := filepath.Join(t.TempDir(), "recovery.sh")
	service := &flexDaemonActivationService{
		provider:       mustNodeProvider(t, "nspawn"),
		log:            slog.Default(),
		paths:          paths,
		state:          &testStateStore{},
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
		provider: mustNodeProvider(t, "nspawn"),
		log:      slog.Default(),
		state:    &testStateStore{},
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
		provider: mustNodeProvider(t, "nspawn"),
		log:      slog.Default(),
		state:    &testStateStore{},
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
				provider:       mustNodeProvider(t, "nspawn"),
				root:           tt.root,
				resolveRoot:    func() string { return tt.now },
				moveUnfinished: func() (bool, error) { return tt.unfinished, tt.unfinishedErr },
				paths:          paths,
				state:          &testStateStore{},
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
		provider:       mustNodeProvider(t, "nspawn"),
		log:            slog.Default(),
		paths:          paths,
		state:          &testStateStore{},
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

func TestFlexDaemonActivationRejectsCorruptAppliedState(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode isolation.Mode
	}{
		{name: "none", mode: isolation.ModeNone},
		{name: "nspawn", mode: isolation.ModeNSpawn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, err := newFileStateStore(filepath.Join(t.TempDir(), stateFileName))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(t.Context(), &runtimeprovider.State{Isolation: tc.mode}); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(store.path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.ReplaceAll(string(data), string(tc.mode), "corrupt"))
			if err := os.WriteFile(store.path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			candidate := filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(candidate, []byte("candidate"), 0o750); err != nil {
				t.Fatal(err)
			}
			service := &flexDaemonActivationService{
				log:            discardLogger(),
				paths:          testAgentUpgradePaths(t),
				state:          store,
				systemdDir:     t.TempDir(),
				recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
				serviceOptions: agentServiceOptions{WantedTargets: (noneProvider{}).WantedTargets()},
				provider:       mustNodeProvider(t, tc.mode),
				inspectService: func(context.Context, *slog.Logger, string) (bool, error) {
					return false, nil
				},
			}
			if _, err := service.Preflight(t.Context(), candidate); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("Preflight accepted corrupt applied state: %v", err)
			}
			if err := service.WaitHealthy(t.Context(), candidate); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("WaitHealthy accepted corrupt applied state: %v", err)
			}
			entries, err := os.ReadDir(service.systemdDir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("activation mutated systemd assets: %v, %v", entries, err)
			}
			if _, err := os.Stat(service.recoveryScript); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("activation wrote recovery assets: %v", err)
			}
		})
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
