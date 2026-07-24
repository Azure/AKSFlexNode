package kubelogin

import (
	"encoding/base64"
	"os"
	"testing"
)

func TestPrepareClientCertificate(t *testing.T) {
	t.Parallel()

	const certificate = "certificate data"
	path, cleanup, err := prepareClientCertificate(base64.StdEncoding.EncodeToString([]byte(certificate)))
	if err != nil {
		t.Fatalf("prepareClientCertificate() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	if string(data) != certificate {
		t.Fatalf("temporary certificate = %q, want %q", data, certificate)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("os.Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("temporary certificate permissions = %o, want 600", info.Mode().Perm())
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary certificate still exists after cleanup: %v", err)
	}
}
