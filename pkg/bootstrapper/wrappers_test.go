package bootstrapper

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCNIConfig_CreatesFile(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	task := WriteCNIConfig(machineDir)

	if task.Name() != "write-cni-config" {
		t.Fatalf("unexpected task name: %s", task.Name())
	}

	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	confPath := filepath.Join(machineDir, "etc", "cni", "net.d", "99-bridge.conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("failed to read config: %v", err)
	}

	if string(data) != string(defaultBridgeCNIConfig) {
		t.Fatalf("config content mismatch:\ngot:  %s\nwant: %s", string(data), string(defaultBridgeCNIConfig))
	}
}

func TestWriteCNIConfig_Idempotent(t *testing.T) {
	t.Parallel()

	machineDir := t.TempDir()
	task := WriteCNIConfig(machineDir)

	// Run twice — second call should be a no-op.
	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := task.Do(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
}
