package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallClientCertificate(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	sourcePath := filepath.Join(sourceDir, "credentials", "client.pem")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	const certificate = "test certificate"
	if err := os.WriteFile(sourcePath, []byte(certificate), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	machineDir := t.TempDir()
	destinationPath := filepath.Join(machineDir, sourcePath)
	if err := os.MkdirAll(filepath.Dir(destinationPath), 0o750); err != nil {
		t.Fatalf("os.MkdirAll: %v", err)
	}
	if err := os.WriteFile(destinationPath, []byte("old"), 0o644); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	task := InstallClientCertificate(sourcePath, machineDir)
	if err := task.Do(t.Context()); err != nil {
		t.Fatalf("InstallClientCertificate().Do() error = %v", err)
	}
	data, err := os.ReadFile(destinationPath)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	if string(data) != certificate {
		t.Fatalf("installed certificate = %q, want %q", data, certificate)
	}
	info, err := os.Stat(destinationPath)
	if err != nil {
		t.Fatalf("os.Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("installed certificate permissions = %o, want 600", info.Mode().Perm())
	}
}
