package utilio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileInRoot(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"replace", "relative directory symlink", "escape", "directory destination"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			path := "units/test.service"
			wantErr := false
			switch name {
			case "replace":
				if err := WriteFileInRoot(root, path, []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "relative directory symlink":
				if err := root.Mkdir("real", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := root.Symlink("real", "units"); err != nil {
					t.Fatal(err)
				}
			case "escape":
				wantErr = true
				outside := t.TempDir()
				if err := root.Symlink(outside, "units"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					entries, err := os.ReadDir(outside)
					if err != nil || len(entries) != 0 {
						t.Errorf("root escape: %v, %v", entries, err)
					}
				})
			case "directory destination":
				wantErr = true
				if err := root.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			err = WriteFileInRoot(root, path, []byte("new"), 0o600)
			if (err != nil) != wantErr {
				t.Fatalf("WriteFileInRoot = %v, want error %v", err, wantErr)
			}
			if name == "escape" {
				return
			}
			entries, err := os.ReadDir(filepath.Join(dir, "units"))
			if err != nil || len(entries) != 1 || entries[0].Name() != "test.service" {
				t.Fatalf("temporary files retained: %v, %v", entries, err)
			}
			if wantErr {
				return
			}
			got, err := root.ReadFile(path)
			if err != nil || string(got) != "new" {
				t.Fatalf("published file = %q, %v", got, err)
			}
			info, err := root.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("permissions = %v, %v", info, err)
			}
		})
	}
}
