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
	"strings"
	"testing"
	"time"
)

func TestValidateClientCertificateFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	validFile := filepath.Join(dir, "client-certificate.pem")
	writeTestClientCertificate(t, validFile)
	insecureFile := filepath.Join(dir, "client-certificate-insecure.pem")
	writeTestClientCertificate(t, insecureFile)
	if err := os.Chmod(insecureFile, 0o644); err != nil {
		t.Fatalf("os.Chmod: %v", err)
	}

	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "valid protected certificate file", path: validFile},
		{
			name:    "rejects insecure permissions",
			path:    insecureFile,
			wantErr: "must not be accessible by group or other users",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := validateClientCertificateFile(tt.path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateClientCertificateFile() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateClientCertificateFile() error = %v", err)
			}
		})
	}
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
