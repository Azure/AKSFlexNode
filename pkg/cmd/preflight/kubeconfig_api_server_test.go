package preflight

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestKubeconfigAPIServerReachable(t *testing.T) {
	t.Parallel()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"api.example.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatalf("generate DNS-only test certificate: %v", err)
	}
	tests := map[string]struct {
		serverName string
		status     int
		wantError  string
	}{
		"TLS name override":           {serverName: "api.example.test", status: http.StatusOK},
		"authentication not required": {serverName: "api.example.test", status: http.StatusUnauthorized},
		"missing TLS name override":   {status: http.StatusOK, wantError: "certificate"},
		"wrong TLS name":              {serverName: "wrong.example", status: http.StatusOK, wantError: "certificate"},
		"server error":                {serverName: "api.example.test", status: http.StatusInternalServerError, wantError: "status 500"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/readyz" {
					t.Errorf("request path = %q, want /readyz", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "" || r.Header.Get("Impersonate-User") != "" {
					t.Error("reachability check sent authentication or impersonation headers")
				}
				w.WriteHeader(tt.status)
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{
				MinVersion: tls.VersionTLS12,
				Certificates: []tls.Certificate{{
					Certificate: [][]byte{certificate},
					PrivateKey:  key,
				}},
			}
			server.StartTLS()
			t.Cleanup(server.Close)

			marker := filepath.Join(t.TempDir(), "exec-called")
			plugin := filepath.Join(t.TempDir(), "container-credential")
			script := fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 1\n", marker)
			if err := os.WriteFile(plugin, []byte(script), 0o700); err != nil {
				t.Fatalf("write test plugin: %v", err)
			}
			kubeconfig := clientcmdapi.NewConfig()
			kubeconfig.CurrentContext = "kubelet"
			kubeconfig.Clusters["cluster"] = &clientcmdapi.Cluster{
				Server:                   server.URL,
				TLSServerName:            tt.serverName,
				CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			}
			kubeconfig.AuthInfos["kubelet"] = &clientcmdapi.AuthInfo{
				Impersonate: "system:node:test",
				Exec: &clientcmdapi.ExecConfig{
					APIVersion:      "client.authentication.k8s.io/v1",
					Command:         plugin,
					InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
				},
			}
			kubeconfig.Contexts["kubelet"] = &clientcmdapi.Context{Cluster: "cluster", AuthInfo: "kubelet"}
			data, err := clientcmd.Write(*kubeconfig)
			if err != nil {
				t.Fatalf("write test kubeconfig: %v", err)
			}

			check := kubeconfigAPIServerReachableChecker{data: string(data)}
			results := check.Check(t.Context())
			if len(results) != 1 || results[0].Name != checkAPIServerReachableName {
				t.Fatalf("Check() results = %#v, want one API reachability result", results)
			}
			if tt.wantError == "" {
				if results[0].Severity != preflight.SeverityOK {
					t.Fatalf("Check() = %#v, want success", results[0])
				}
			} else if results[0].Severity != preflight.SeverityError || !strings.Contains(results[0].Message, tt.wantError) {
				t.Fatalf("Check() = %#v, want error containing %q", results[0], tt.wantError)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("kubelet credential plugin ran, marker stat error = %v", err)
			}
		})
	}
}
