package daemon

import (
	"path/filepath"
	"testing"
)

func assertResolvedPath(t *testing.T, path, want string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	if got != want {
		t.Fatalf("resolved %s = %s, want %s", path, got, want)
	}
}
