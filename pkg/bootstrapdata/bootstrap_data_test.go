package bootstrapdata

import (
	"context"
	"io"
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

func (staticCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "arm-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestFetchAndWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	output := filepath.Join(dir, "bootstrap-data.json")
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
		if request.Header.Get("Authorization") != "Bearer arm-token" {
			t.Error("missing token")
		}
		body := `{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	options := Options{
		ClusterResourceID: "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		AgentPoolName:     "aksflexnodes", AuthMode: "msi",
		ResourceManagerEndpoint: DefaultResourceManagerEndpoint,
		AuthorityHost:           DefaultAuthorityHost, APIVersion: DefaultAPIVersion, OutputPath: output,
	}
	err := fetchAndWrite(context.Background(), options, dependencies{
		credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) { return staticCredential{}, nil },
		httpClient: client,
	})
	if err != nil {
		t.Fatalf("fetchAndWrite() error = %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestFetchInMemory(t *testing.T) {
	t.Parallel()

	const response = `{
		"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}},
		"node":{"kubelet":{"clusterFQDN":"api.example.test","caCertData":"Y2E="}}
	}`
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(response)),
			Header:     make(http.Header),
		}, nil
	})}
	options := Options{
		ClusterResourceID:       "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		AgentPoolName:           "aksflexnodes",
		AuthMode:                "msi",
		ResourceManagerEndpoint: DefaultResourceManagerEndpoint,
		AuthorityHost:           DefaultAuthorityHost,
		APIVersion:              DefaultAPIVersion,
	}
	got, err := fetch(t.Context(), options, dependencies{
		credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) { return staticCredential{}, nil },
		httpClient: client,
	})
	if err != nil {
		t.Fatalf("fetch() error = %v", err)
	}
	if got.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("BootstrapToken = %q", got.BootstrapToken)
	}
	if got.ClusterFQDN != "api.example.test" {
		t.Fatalf("ClusterFQDN = %q", got.ClusterFQDN)
	}
	if got.CACertData != "Y2E=" {
		t.Fatalf("CACertData = %q", got.CACertData)
	}
}

func TestFetchAndWriteRequiresOutput(t *testing.T) {
	t.Parallel()

	err := fetchAndWrite(t.Context(), Options{}, dependencies{})
	if err == nil || err.Error() != "output path is required" {
		t.Fatalf("fetchAndWrite() error = %v, want output path error", err)
	}
}

func TestClientCertificateCredentialOptions(t *testing.T) {
	t.Parallel()
	options := clientCertificateCredentialOptions(azcore.ClientOptions{})
	if !options.SendCertificateChain {
		t.Fatal("SendCertificateChain = false")
	}
}
