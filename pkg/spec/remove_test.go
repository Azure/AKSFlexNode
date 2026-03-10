package spec

import (
	"os"
	"testing"
)

func TestRemoveManagedClusterSpecSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := ManagedClusterSpecFilePath(dir)

	// No file: should be (removed=false, err=nil).
	removed, err := RemoveManagedClusterSpecSnapshotAtPath(path)
	if err != nil {
		t.Fatalf("RemoveManagedClusterSpecSnapshot() err=%v, want nil", err)
	}
	if removed {
		t.Fatalf("RemoveManagedClusterSpecSnapshot() removed=true, want false")
	}

	// Create the file and remove it.
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	removed, err = RemoveManagedClusterSpecSnapshotAtPath(path)
	if err != nil {
		t.Fatalf("RemoveManagedClusterSpecSnapshot() err=%v, want nil", err)
	}
	if !removed {
		t.Fatalf("RemoveManagedClusterSpecSnapshot() removed=false, want true")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("spec file still exists; statErr=%v", statErr)
	}
}
