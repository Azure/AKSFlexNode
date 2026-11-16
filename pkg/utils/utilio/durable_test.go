package utilio

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileDurable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "nested", "journal.json")
	for _, content := range []string{"first", "second", "second"} {
		if err := WriteFileDurable(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("content = %q, error = %v", got, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
}

func TestWriteFileDurableRejectsFileParent(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileDurable(filepath.Join(parent, "journal"), nil, 0o600); err == nil {
		t.Fatal("expected non-directory parent error")
	}
}
