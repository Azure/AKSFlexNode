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

type errorReadCloser struct {
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

func (r *errorReadCloser) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (r *errorReadCloser) Close() error {
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

	const statusOnly = "fetch bootstrap data returned HTTP status 403"
	boundedCode := strings.Repeat("C", maxARMErrorCodeBytes)
	boundedMessage := strings.Repeat("M", maxARMErrorMessageBytes)
	boundedRequestID := strings.Repeat("R", maxARMRequestIDBytes)
	tests := []struct {
		name        string
		body        string
		headers     http.Header
		want        string
		notContains []string
	}{
		{
			name: "structured ARM error",
			body: `{"error":{"code":"AuthorizationFailed","message":"The caller is not authorized."}}`,
			headers: http.Header{
				"X-Ms-Request-Id":             []string{"request-123"},
				"X-Ms-Correlation-Request-Id": []string{"correlation-456"},
			},
			want: statusOnly + `: error.code="AuthorizationFailed", error.message="The caller is not authorized.", x-ms-request-id="request-123", x-ms-correlation-request-id="correlation-456"`,
		},
		{
			name: "missing message and request IDs",
			body: `{"error":{"code":"Forbidden"}}`,
			want: statusOnly + `: error.code="Forbidden"`,
		},
		{
			name: "missing code",
			body: `{"error":{"message":"Access denied"}}`,
			want: statusOnly + `: error.message="Access denied"`,
		},
		{
			name:        "malformed JSON",
			body:        `{"error":{"message":"arm-token and body-secret"}`,
			headers:     http.Header{"X-Ms-Request-Id": []string{"malformed-request"}},
			want:        statusOnly + `: x-ms-request-id="malformed-request"`,
			notContains: []string{"arm-token", "body-secret"},
		},
		{
			name:        "unrelated JSON",
			body:        `{"message":"body-secret","bootstrapToken":"bootstrap-secret"}`,
			headers:     http.Header{"X-Ms-Correlation-Request-Id": []string{"unrelated-correlation"}},
			want:        statusOnly + `: x-ms-correlation-request-id="unrelated-correlation"`,
			notContains: []string{"body-secret", "bootstrap-secret"},
		},
		{
			name:        "invalid UTF-8",
			body:        string([]byte{0xff, 0xfe}),
			headers:     http.Header{"X-Ms-Request-Id": []string{"invalid-utf8-request"}},
			want:        statusOnly + `: x-ms-request-id="invalid-utf8-request"`,
			notContains: []string{string([]byte{0xff, 0xfe})},
		},
		{
			name: "unrelated secrets omitted and bearer token redacted",
			body: `{"error":{"code":"AuthorizationFailed","message":"token arm-token was rejected"},` +
				`"bootstrapToken":"bootstrap-secret","secret":"body-secret"}`,
			want: statusOnly + `: error.code="AuthorizationFailed", error.message="token [REDACTED] was rejected"`,
			notContains: []string{
				"arm-token",
				"bootstrap-secret",
				"body-secret",
			},
		},
		{
			name: "oversized fields are bounded",
			body: `{"error":{"code":"` + boundedCode + `code-secret","message":"` +
				boundedMessage + `message-secret"}}`,
			want: statusOnly + `: error.code="` + boundedCode + `", error.message="` + boundedMessage + `"`,
			notContains: []string{
				"code-secret",
				"message-secret",
			},
		},
		{
			name: "oversized body uses request ID only",
			body: `{"error":{"code":"Forbidden","message":"` +
				strings.Repeat("M", int(maxErrorResponseBytes)) + `body-secret"}}`,
			headers:     http.Header{"X-Ms-Request-Id": []string{"oversized-request"}},
			want:        statusOnly + `: x-ms-request-id="oversized-request"`,
			notContains: []string{"Forbidden", "body-secret"},
		},
		{
			name: "request IDs are bounded sanitized and redacted",
			body: `{"error":{"code":"Forbidden"}}`,
			headers: http.Header{
				"X-Ms-Request-Id":             []string{boundedRequestID + "request-secret"},
				"X-Ms-Correlation-Request-Id": []string{"correlation\r\narm-token\tvalue"},
			},
			want: statusOnly + `: error.code="Forbidden", x-ms-request-id="` + boundedRequestID +
				`", x-ms-correlation-request-id="correlation  [REDACTED] value"`,
			notContains: []string{
				"arm-token",
				"request-secret",
				"\r",
				"\n",
				"\t",
			},
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
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			body := &trackingReadCloser{Reader: strings.NewReader(test.body)}
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Body:       body,
					Header:     test.headers.Clone(),
				}, nil
			})}
			_, err := fetch(t.Context(), options, dependencies{
				credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) {
					return staticCredential{}, nil
				},
				httpClient: client,
			})
			if err == nil || err.Error() != test.want {
				t.Fatalf("fetch() error = %v, want %q", err, test.want)
			}
			for _, value := range test.notContains {
				if strings.Contains(err.Error(), value) {
					t.Errorf("fetch() error exposed %q", value)
				}
			}
			if !body.closed {
				t.Error("fetch() did not close the error response body")
			}
		})
	}

	err := bootstrapDataHTTPError(&http.Response{
		StatusCode: http.StatusServiceUnavailable,
		Header:     http.Header{"X-Ms-Request-Id": []string{"bodyless-request"}},
	}, "arm-token")
	const wantBodyless = `fetch bootstrap data returned HTTP status 503: x-ms-request-id="bodyless-request"`
	if err.Error() != wantBodyless {
		t.Fatalf("bodyless HTTP error = %q, want %q", err, wantBodyless)
	}

	readFailureBody := &errorReadCloser{}
	err = bootstrapDataHTTPError(&http.Response{
		StatusCode: http.StatusBadGateway,
		Body:       readFailureBody,
		Header:     http.Header{"X-Ms-Correlation-Request-Id": []string{"read-error-correlation"}},
	}, "arm-token")
	const wantReadFailure = `fetch bootstrap data returned HTTP status 502: x-ms-correlation-request-id="read-error-correlation"`
	if err.Error() != wantReadFailure {
		t.Fatalf("read-failure HTTP error = %q, want %q", err, wantReadFailure)
	}
	if !readFailureBody.closed {
		t.Error("read-failure HTTP error did not close its response body")
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
