package kubelogin

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrepareClientCertificate(t *testing.T) {
	t.Parallel()

	certificateFile := filepath.Join(t.TempDir(), "client-certificate.pem")
	writeTestClientCertificate(t, certificateFile)
	path, cleanup, err := prepareClientCertificate(certificateFile)
	if err != nil {
		t.Fatalf("prepareClientCertificate() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("temporary certificate should not be empty")
	}
	if string(data) == "" || !containsPEMCertificate(data) || !containsPEMPrivateKey(data) {
		t.Fatalf("temporary certificate should contain normalized PEM certificate and private key, got %q", data)
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

func containsPEMCertificate(data []byte) bool {
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		if block.Type == "CERTIFICATE" {
			return true
		}
	}
	return false
}

func containsPEMPrivateKey(data []byte) bool {
	for len(data) > 0 {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return false
		}
		if block.Type == "PRIVATE KEY" {
			return true
		}
	}
	return false
}

func writeTestClientCertificate(t *testing.T, path string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-client"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	privateKeyData, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatalf("x509.MarshalPKCS8PrivateKey: %v", err)
	}
	data := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyData})...,
	)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}
}
