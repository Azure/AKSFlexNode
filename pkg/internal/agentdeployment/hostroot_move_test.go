package agentdeployment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostRootMoveUnfinished(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup puts something at root; dir is a scratch directory.
		setup   func(t *testing.T, root, dir string)
		want    bool
		wantErr bool
	}{
		{name: "no host root", setup: func(*testing.T, string, string) {}},
		{
			name: "a finished installation",
			setup: func(t *testing.T, root, _ string) {
				if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a move that has not finished",
			setup: func(t *testing.T, root, _ string) {
				if err := os.MkdirAll(root, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, hostRootMovingMarker), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: true,
		},
		{
			// A host linked to the legacy root, even one whose target holds
			// a file of that name, is not moving.
			name: "a host linked to the legacy root",
			setup: func(t *testing.T, root, dir string) {
				legacy := filepath.Join(dir, "legacy")
				if err := os.MkdirAll(legacy, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacy, hostRootMovingMarker), nil, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(legacy, root); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a file in its place",
			setup: func(t *testing.T, root, _ string) {
				if err := os.WriteFile(root, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// Its parent is a file, so it cannot be inspected.
			name: "an unreadable host root",
			setup: func(t *testing.T, root, _ string) {
				if err := os.Remove(filepath.Dir(root)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Dir(root), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			root := filepath.Join(dir, "opt", "unbounded", "agent")
			if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, root, dir)

			got, err := hostRootMoveUnfinished(root)
			if (err != nil) != tt.wantErr {
				t.Fatalf("hostRootMoveUnfinished() error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("hostRootMoveUnfinished() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")

	tests := []struct {
		name    string
		dirs    []string
		wantErr string
	}{
		{name: "nothing to sync"},
		{name: "existing directories, repeated", dirs: []string{dir, dir, t.TempDir()}},
		{name: "a missing directory is reported", dirs: []string{dir, missing}, wantErr: missing},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := SyncDirs(tt.dirs...)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("SyncDirs() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("SyncDirs() error = %v, want one naming %s", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyMovedAgent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths := UpgradePathsUnder(root)
	if err := os.MkdirAll(filepath.Dir(paths.CurrentPath), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := VerifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("VerifyMovedAgent() passed a layout with no current binary")
	}

	writeExecutable(t, paths.GreenPath, "#!/bin/sh\n[ \"$1\" = version ]\n")
	if err := os.Symlink(paths.GreenPath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMovedAgent(t.Context(), root); err != nil {
		t.Fatalf("VerifyMovedAgent() error = %v", err)
	}

	if err := os.Chmod(paths.GreenPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMovedAgent(t.Context(), root); err == nil {
		t.Fatal("VerifyMovedAgent() passed a binary that cannot run")
	}
}
