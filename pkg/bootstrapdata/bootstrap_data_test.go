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

type staticCredential struct {
	scope *string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (c staticCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if c.scope != nil && len(options.Scopes) == 1 {
		*c.scope = options.Scopes[0]
	}
	return azcore.AccessToken{Token: "arm-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

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
		ResourceManagerEndpoint: "https://management.usgovcloudapi.net",
		ResourceManagerAudience: "https://management.core.usgovcloudapi.net",
		AuthorityHost:           "https://login.microsoftonline.us/",
		APIVersion:              DefaultAPIVersion,
	}
	var scope string
	got, err := fetch(t.Context(), options, dependencies{
		credential: func(_ Options, clientOptions azcore.ClientOptions) (azcore.TokenCredential, error) {
			service := clientOptions.Cloud.Services["resourceManager"]
			if service.Audience != options.ResourceManagerAudience {
				t.Errorf("ARM audience = %q, want %q", service.Audience, options.ResourceManagerAudience)
			}
			return staticCredential{scope: &scope}, nil
		},
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
	if scope != "https://management.core.usgovcloudapi.net/.default" {
		t.Fatalf("ARM token scope = %q", scope)
	}
}

func TestFetchHTTPErrorDiagnostics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		body        string
		headers     http.Header
		want        []string
		notContains []string
	}{
		{
			name: "structured ARM error",
			body: `{"error":{"code":"AuthorizationFailed","message":"The caller is not authorized."}}`,
			headers: http.Header{
				"X-Ms-Request-Id":             []string{"request-123"},
				"X-Ms-Correlation-Request-Id": []string{"correlation-456"},
			},
			want: []string{
				"fetch bootstrap data returned HTTP status 403",
				`error.code="AuthorizationFailed"`,
				`error.message="The caller is not authorized."`,
				`x-ms-request-id="request-123"`,
				`x-ms-correlation-request-id="correlation-456"`,
			},
		},
		{
			name:        "unrelated response fields are omitted",
			body:        `{"message":"body-secret","bootstrapToken":"bootstrap-secret"}`,
			want:        []string{"fetch bootstrap data returned HTTP status 403"},
			notContains: []string{"body-secret", "bootstrap-secret"},
		},
		{
			name: "bearer token and control characters are sanitized",
			body: `{"error":{"code":"Forbidden","message":"token arm-token\nwas rejected"}}`,
			want: []string{
				`error.code="Forbidden"`,
				`error.message="token [REDACTED] was rejected"`,
			},
			notContains: []string{"arm-token", "\n"},
		},
	}

	options := Options{
		ClusterResourceID:       "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		AgentPoolName:           "aksflexnodes",
		AuthMode:                "msi",
		ResourceManagerEndpoint: DefaultResourceManagerEndpoint,
		AuthorityHost:           DefaultAuthorityHost,
		APIVersion:              DefaultAPIVersion,
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := &trackingReadCloser{Reader: strings.NewReader(tt.body)}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       body,
					Header:     tt.headers.Clone(),
				}, nil
			})}
			_, err := fetch(t.Context(), options, dependencies{
				credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) {
					return staticCredential{}, nil
				},
				httpClient: client,
			})
			if err == nil {
				t.Fatal("fetch() error = nil, want HTTP error")
			}
			for _, value := range tt.want {
				if !strings.Contains(err.Error(), value) {
					t.Errorf("fetch() error = %q, want it to contain %q", err, value)
				}
			}
			for _, value := range tt.notContains {
				if strings.Contains(err.Error(), value) {
					t.Errorf("fetch() error exposed %q", value)
				}
			}
			if !body.closed {
				t.Error("fetch() did not close the error response body")
			}
		})
	}
}

func TestFetchRejectsMalformedBootstrapToken(t *testing.T) {
	t.Parallel()

	const malformedToken = "truncated.token"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		body := `{"azure":{"bootstrapToken":{"token":"` + malformedToken + `"}}}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
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
	_, err := fetch(t.Context(), options, dependencies{
		credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) { return staticCredential{}, nil },
		httpClient: client,
	})
	if err == nil || err.Error() != "bootstrap-data response contained an invalid bootstrap token" {
		t.Fatalf("fetch() error = %v, want invalid token error", err)
	}
	if strings.Contains(err.Error(), malformedToken) {
		t.Fatal("fetch() error exposed malformed bootstrap token")
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
