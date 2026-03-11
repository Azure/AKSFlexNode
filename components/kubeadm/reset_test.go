package kubeadm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveDirContents_RemovesChildrenKeepsRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root := filepath.Join(dir, "kubelet")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	childFile := filepath.Join(root, "file.txt")
	if err := os.WriteFile(childFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write child file: %v", err)
	}

	childDir := filepath.Join(root, "subdir")
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatalf("mkdir child dir: %v", err)
	}

	if err := removeDirContents(root); err != nil {
		t.Fatalf("removeDirContents: %v", err)
	}

	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root should still exist, stat: %v", err)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("readdir root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected root to be empty, got %d entries", len(entries))
	}
}

func TestRemoveDirContents_NotExistIsOK(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")
	if err := removeDirContents(dir); err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
}
