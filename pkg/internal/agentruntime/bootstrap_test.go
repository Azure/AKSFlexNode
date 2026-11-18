package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestBootstrapArtifactConflicts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		mode      isolation.Mode
		installed bool
		artifacts bootstrapArtifacts
		wantErr   string
	}{
		{
			name: "orphan OCI",
			mode: isolation.ModeContainer,
			artifacts: bootstrapArtifacts{
				oci: true,
			},
			wantErr: "OCI worker has no applied state",
		},
		{
			name:      "none conflicts with OCI",
			mode:      isolation.ModeContainer,
			installed: true,
			artifacts: bootstrapArtifacts{
				none: true,
				oci:  true,
			},
			wantErr: "host-native worker is present",
		},
		{
			name: "orphan none",
			mode: isolation.ModeNone,
			artifacts: bootstrapArtifacts{
				none: true,
			},
			wantErr: "host-native worker has no applied state",
		},
		{
			name: "OCI conflicts with none",
			mode: isolation.ModeNone,
			artifacts: bootstrapArtifacts{
				oci: true,
			},
			wantErr: "OCI worker is present",
		},
		{
			name: "OCI conflicts with nspawn",
			mode: isolation.ModeNSpawn,
			artifacts: bootstrapArtifacts{
				oci: true,
			},
			wantErr: "OCI worker is present",
		},
		{
			name: "none conflicts with nspawn",
			mode: isolation.ModeNSpawn,
			artifacts: bootstrapArtifacts{
				none: true,
			},
			wantErr: "host-native worker is present",
		},
		{
			name: "both conflict with nspawn",
			mode: isolation.ModeNSpawn,
			artifacts: bootstrapArtifacts{
				none: true,
				oci:  true,
			},
			wantErr: "OCI worker is present",
		},
		{
			name:      "owned OCI artifacts",
			mode:      isolation.ModeContainer,
			installed: true,
			artifacts: bootstrapArtifacts{
				oci: true,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			ensureClean := func(context.Context, *slog.Logger, isolation.Mode) error {
				calls++
				return errors.New("unexpected clean-host check")
			}
			log := slog.New(slog.DiscardHandler)
			var err error
			switch tc.mode {
			case isolation.ModeNSpawn:
				err = validateNSpawnBootstrap(tc.artifacts)
			case isolation.ModeNone:
				err = validateNoneBootstrap(t.Context(), log, tc.installed, tc.artifacts, none.NewDefault(log), ensureClean)
			case isolation.ModeContainer:
				err = validateContainerBootstrap(t.Context(), log, tc.installed, tc.artifacts, ensureClean)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected artifact validation error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("artifact validation error = %v, want %q", err, tc.wantErr)
			}
			if calls != 0 {
				t.Fatalf("unexpected clean-host calls = %d", calls)
			}
		})
	}
}

func TestNoneBootstrapChecksOwnershipAfterCleanHost(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kubelet.service"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &none.Backend{
		Host: &none.HostFS{
			UnitDirs: []string{dir},
		},
	}
	calls := 0
	ensureClean := func(context.Context, *slog.Logger, isolation.Mode) error {
		calls++
		return nil
	}
	err := validateNoneBootstrap(t.Context(), slog.New(slog.DiscardHandler), true, bootstrapArtifacts{}, backend, ensureClean)
	if err == nil || !strings.Contains(err.Error(), "not managed by the agent") {
		t.Fatalf("ownership error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("clean-host calls = %d, want 1", calls)
	}
}

func TestBootstrapCleanHostGuard(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		mode      isolation.Mode
		installed bool
		cleanErr  error
		wantClean bool
		wantErr   string
	}{
		{
			name:      "fresh OCI clean host",
			mode:      isolation.ModeContainer,
			wantClean: true,
		},
		{
			name:      "fresh OCI bpffs conflict",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("bpffs mount path /run/bpffs/kube1"),
			wantClean: true,
			wantErr:   "bpffs mount path",
		},
		{
			name:      "fresh OCI nspawn conflict",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("registered nspawn machine kube2"),
			wantClean: true,
			wantErr:   "registered nspawn machine",
		},
		{
			name:      "fresh OCI inspection error",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("artifact cannot be inspected"),
			wantClean: true,
			wantErr:   "artifact cannot be inspected",
		},
		{
			name:      "installed OCI owns its bpffs",
			mode:      isolation.ModeContainer,
			installed: true,
			cleanErr:  errors.New("owned bpffs mount path"),
		},
		{
			name:      "none still checks clean host",
			mode:      isolation.ModeNone,
			installed: true,
			cleanErr:  errors.New("nspawn artifact"),
			wantClean: true,
			wantErr:   "nspawn artifact",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log := slog.New(slog.DiscardHandler)
			calls := 0
			ensureClean := func(ctx context.Context, gotLog *slog.Logger, mode isolation.Mode) error {
				calls++
				if ctx != t.Context() || gotLog != log || mode != isolation.ModeNSpawn {
					t.Fatalf("clean-host check received unexpected inputs: mode=%q", mode)
				}
				return tc.cleanErr
			}

			var err error
			if tc.mode == isolation.ModeContainer {
				err = validateContainerBootstrap(t.Context(), log, tc.installed, bootstrapArtifacts{}, ensureClean)
			} else {
				err = validateNoneBootstrap(t.Context(), log, tc.installed, bootstrapArtifacts{}, none.NewDefault(log), ensureClean)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validation error = %v, want %q", err, tc.wantErr)
			}
			wantCalls := 0
			if tc.wantClean {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("clean-host calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}
