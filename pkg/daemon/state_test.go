package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStateStoreSaveLoad(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	want := &State{
		AppliedSettingsVersion:    "42",
		AppliedKubernetesVersion:  "1.34.0",
		PreviousSettingsVersion:   "41",
		PreviousKubernetesVersion: "1.33.0",
		ActiveMachine:             "kube2",
	}

	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AppliedSettingsVersion != want.AppliedSettingsVersion || got.ActiveMachine != want.ActiveMachine {
		t.Fatalf("state = %#v, want %#v", got, want)
	}
}

func TestFileStateStoreLoadMissing(t *testing.T) {
	t.Parallel()

	store, err := newFileStateStore(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != nil {
		t.Fatalf("state = %#v, want nil", got)
	}
}

func TestFileStateStoreChecksumMismatch(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"appliedSettingsVersion":"43"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err = store.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Load error = %v, want checksum mismatch", err)
	}
}

func TestFileStateStoreCorruptJSON(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	data := []byte(`{"appliedSettingsVersion":`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(path+".sha256", []byte(checksum(data)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile checksum: %v", err)
	}
	_, err = store.Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "decode daemon state") {
		t.Fatalf("Load error = %v, want decode error", err)
	}
}

func TestFileStateStoreDelete(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.json")
	store, err := newFileStateStore(path)
	if err != nil {
		t.Fatalf("newFileStateStore: %v", err)
	}
	if err := store.Save(context.Background(), &State{AppliedSettingsVersion: "42"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Delete(context.Background()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file exists after Delete: %v", err)
	}
	if _, err := os.Stat(path + ".sha256"); !os.IsNotExist(err) {
		t.Fatalf("checksum file exists after Delete: %v", err)
	}
}
