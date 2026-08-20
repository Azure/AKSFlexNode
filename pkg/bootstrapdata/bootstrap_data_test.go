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

func (c staticCredential) GetToken(_ context.Context, options policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if c.scope != nil && len(options.Scopes) == 1 {
		*c.scope = options.Scopes[0]
	}
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
		transport:  client,
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

	const apiVersion = "2026-06-02-preview"
	const response = `{
		"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}},
		"node":{"kubelet":{"clusterFQDN":"api.example.test","caCertData":"Y2E="}}
	}`
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost {
			t.Errorf("request method = %q", request.Method)
		}
		if request.URL.Host != "management.usgovcloudapi.net" {
			t.Errorf("request host = %q", request.URL.Host)
		}
		if request.URL.Path != "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster/agentPools/aksflexnodes/listBootstrapData" {
			t.Errorf("request path = %q", request.URL.Path)
		}
		if request.URL.Query().Get("api-version") != apiVersion {
			t.Errorf("api-version = %q", request.URL.Query().Get("api-version"))
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
		}
		requestBody, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(requestBody) != "{}" {
			t.Errorf("request body = %q, want {}", requestBody)
		}
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
		APIVersion:              apiVersion,
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
		transport: client,
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
		transport:  client,
	})
	if err == nil || err.Error() != "bootstrap-data response contained an invalid bootstrap token" {
		t.Fatalf("fetch() error = %v, want invalid token error", err)
	}
	if strings.Contains(err.Error(), malformedToken) {
		t.Fatal("fetch() error exposed malformed bootstrap token")
	}
}

func TestFetchRetriesTooManyRequests(t *testing.T) {
	t.Parallel()

	const responseBody = `{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader("throttled")),
				Header:     make(http.Header),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Header:     make(http.Header),
		}, nil
	})}
	got, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential:   staticCredentialFactory,
		transport:    client,
		retryOptions: noDelayRetryOptions(1),
	})
	if err != nil {
		t.Fatalf("fetch() error = %v", err)
	}
	if got.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("BootstrapToken = %q", got.BootstrapToken)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestFetchDoesNotRetryOtherStatusCodes(t *testing.T) {
	t.Parallel()

	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("unavailable")),
			Header:     http.Header{"Retry-After": []string{"1"}},
		}, nil
	})}
	_, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential:   staticCredentialFactory,
		transport:    client,
		retryOptions: noDelayRetryOptions(3),
	})
	if err == nil {
		t.Fatal("fetch() error = nil, want HTTP 503 response error")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
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

func validTestOptions() Options {
	return Options{
		ClusterResourceID:       "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		AgentPoolName:           "aksflexnodes",
		AuthMode:                "msi",
		ResourceManagerEndpoint: DefaultResourceManagerEndpoint,
		AuthorityHost:           DefaultAuthorityHost,
		APIVersion:              DefaultAPIVersion,
	}
}

func staticCredentialFactory(Options, azcore.ClientOptions) (azcore.TokenCredential, error) {
	return staticCredential{}, nil
}

func noDelayRetryOptions(maxRetries int32) *policy.RetryOptions {
	return &policy.RetryOptions{
		MaxRetries:    maxRetries,
		RetryDelay:    time.Nanosecond,
		MaxRetryDelay: time.Nanosecond,
		StatusCodes:   []int{http.StatusTooManyRequests},
	}
}
