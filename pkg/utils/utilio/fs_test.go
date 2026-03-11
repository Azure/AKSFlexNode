package utilio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		setup   func(t *testing.T) string // returns the directory path to clean
		verify  func(t *testing.T, path string)
		wantErr bool
	}{
		{
			name: "non-existent directory returns nil",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "does-not-exist")
			},
			verify: func(t *testing.T, path string) {
				// directory should still not exist
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("expected directory to still not exist, got err: %v", err)
				}
			},
		},
		{
			name: "empty directory remains intact",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			verify: func(t *testing.T, path string) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("directory should still exist: %v", err)
				}
				if !info.IsDir() {
					t.Fatalf("expected a directory, got file")
				}
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "removes files from directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				for _, name := range []string{"a.txt", "b.txt", "c.log"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("content"), 0644); err != nil {
						t.Fatalf("setup: %v", err)
					}
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "removes subdirectories recursively",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				nested := filepath.Join(dir, "sub", "deep")
				if err := os.MkdirAll(nested, 0755); err != nil {
					t.Fatalf("setup: %v", err)
				}
				if err := os.WriteFile(filepath.Join(nested, "file.txt"), []byte("deep"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "removes mixed files and directories",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				// create files
				if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				// create subdirectory with files
				sub := filepath.Join(dir, "subdir")
				if err := os.MkdirAll(sub, 0755); err != nil {
					t.Fatalf("setup: %v", err)
				}
				if err := os.WriteFile(filepath.Join(sub, "nested.txt"), []byte("nested"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				// create empty subdirectory
				if err := os.MkdirAll(filepath.Join(dir, "empty-sub"), 0755); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "preserves the directory itself",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("directory should still exist: %v", err)
				}
				if !info.IsDir() {
					t.Fatalf("expected path to remain a directory")
				}
			},
		},
		{
			name: "idempotent on already empty directory",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			verify: func(t *testing.T, path string) {
				// call CleanDir a second time to verify idempotency
				if err := CleanDir(path); err != nil {
					t.Fatalf("second CleanDir call failed: %v", err)
				}
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "idempotent on non-existent directory",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "gone")
			},
			verify: func(t *testing.T, path string) {
				// call CleanDir a second time to verify idempotency
				if err := CleanDir(path); err != nil {
					t.Fatalf("second CleanDir call failed: %v", err)
				}
			},
		},
		{
			name: "path is a regular file returns error",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				fname := filepath.Join(dir, "notadir.txt")
				if err := os.WriteFile(fname, []byte("I am a file"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return fname
			},
			wantErr: true,
		},
		{
			name: "removes hidden dotfiles",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("public"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
		{
			name: "removes symlinks without following them",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				// create a target file outside the directory
				target := filepath.Join(t.TempDir(), "target.txt")
				if err := os.WriteFile(target, []byte("target"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				// create a symlink inside the directory
				if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return dir
			},
			verify: func(t *testing.T, path string) {
				entries, err := os.ReadDir(path)
				if err != nil {
					t.Fatalf("failed to read directory: %v", err)
				}
				if len(entries) != 0 {
					t.Fatalf("expected empty directory, got %d entries", len(entries))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := tt.setup(t)
			err := CleanDir(path)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.verify != nil {
				tt.verify(t, path)
			}
		})
	}
}
