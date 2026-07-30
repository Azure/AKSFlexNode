package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type staticCredential struct{}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (staticCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "arm-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestBuildConfigFetchAndOverrides(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || !strings.Contains(request.URL.Path, "/agentPools/aksflexnodes/listBootstrapData") {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer arm-token" {
			t.Error("missing bearer token")
		}
		body := `{
			"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}},
			"components":{"kubernetes":"1.36.2","containerd":"stale","runc":"stale"},
			"networking":{"dnsServiceIP":"10.0.0.10","cniVersion":"stale"},
			"node":{"kubelet":{"clusterFQDN":"api.example.test:443","caCertData":"Y2E="}}
		}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}

	resourceID := "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster"
	data, err := buildConfig(context.Background(), Options{
		AuthMode:                        "msi",
		FetchBootstrapData:              true,
		BootstrapDataAPIVersion:         defaultBootstrapDataAPIVersion,
		ClusterResourceID:               resourceID,
		AgentPoolName:                   "aksflexnodes",
		ResourceManagerEndpoint:         defaultResourceManagerEndpoint,
		BootstrapOCIImage:               "https://mirror.example/rootfs.tar.gz",
		BootstrapOfflineArtifactsSource: "https://mirror.example/bundle-{{ .KubernetesVersion }}.tar.gz",
		ConfigOverrides:                 []string{`{"azure":{"targetAgentPoolName":"wrong"},"node":{"labels":{"test":"true"}}}`},
	}, dependencies{
		credential: func(object, Options) (azcore.TokenCredential, error) { return staticCredential{}, nil },
		httpClient: client,
	})
	if err != nil {
		t.Fatalf("buildConfig() error = %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	azure := result["azure"].(map[string]any)
	if azure["targetAgentPoolName"] != "aksflexnodes" {
		t.Fatalf("pool = %v", azure["targetAgentPoolName"])
	}
	if _, ok := azure["managedIdentity"]; !ok {
		t.Fatal("managedIdentity is missing")
	}
	components := result["components"].(map[string]any)
	if _, ok := components["containerd"]; ok {
		t.Fatal("containerd pin was not removed")
	}
	if _, ok := components["runc"]; ok {
		t.Fatal("runc pin was not removed")
	}
	networking := result["networking"].(map[string]any)
	if _, ok := networking["cniVersion"]; ok {
		t.Fatal("CNI pin was not removed")
	}
	bootstrap := result["bootstrap"].(map[string]any)
	if bootstrap["ociImage"] != "https://mirror.example/rootfs.tar.gz" {
		t.Fatal("OCI override is missing")
	}
}

func TestBuildConfigCertificateFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	certificatePath := filepath.Join(dir, "credential-without-extension")
	writeCertificate(t, certificatePath)
	basePath := filepath.Join(dir, "base.json")
	base := `{
		"azure":{"targetAgentPoolName":"aksflexnodes","targetCluster":{"resourceId":"/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster"},"bootstrapToken":{"token":"abcdef.0123456789abcdef"}},
		"components":{"kubernetes":"1.36.2"},
		"node":{"kubelet":{"clusterFQDN":"api.example.test:443","caCertData":"Y2E="}}
	}`
	if err := os.WriteFile(basePath, []byte(base), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := BuildConfig(context.Background(), Options{
		AuthMode:                "service-principal",
		SPTenantID:              "tenant",
		SPClientID:              "client",
		SPClientCertificateFile: certificatePath,
		BaseConfigPath:          basePath,
	})
	if err != nil {
		t.Fatalf("BuildConfig() error = %v", err)
	}
	var result struct {
		Azure struct {
			ServicePrincipal map[string]any `json:"servicePrincipal"`
		} `json:"azure"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Azure.ServicePrincipal["clientSecretFile"] != certificatePath {
		t.Fatalf("service principal = %#v", result.Azure.ServicePrincipal)
	}
	if _, ok := result.Azure.ServicePrincipal["clientSecret"]; ok {
		t.Fatal("inline secret is present")
	}
}

func writeCertificate(t *testing.T, path string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "bootstrap-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyData, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	data := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyData})...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
