package aksmachine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

func TestARMProxyTransportRewritesRequest(t *testing.T) {
	t.Parallel()

	transport, err := newARMProxyTransport("http://127.0.0.1:8080/proxy?proxy=true", roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got, want := req.URL.String(), "http://127.0.0.1:8080/proxy/subscriptions/123/resourceGroups/rg?proxy=true&api-version=2026-01-01"; got != want {
			t.Fatalf("proxied URL = %q, want %q", got, want)
		}
		if got, want := req.Host, "127.0.0.1:8080"; got != want {
			t.Fatalf("Host = %q, want %q", got, want)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	}))
	if err != nil {
		t.Fatalf("newARMProxyTransport() error = %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://management.azure.com/subscriptions/123/resourceGroups/rg?api-version=2026-01-01", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	if _, err := transport.Do(req); err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if got, want := req.URL.String(), "https://management.azure.com/subscriptions/123/resourceGroups/rg?api-version=2026-01-01"; got != want {
		t.Fatalf("original URL = %q, want %q", got, want)
	}
}

func TestARMProxyTransportRejectsInvalidURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		proxyURL string
	}{
		{name: "relative", proxyURL: "/proxy"},
		{name: "unsupported scheme", proxyURL: "ftp://127.0.0.1/proxy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := newARMProxyTransport(tt.proxyURL, nil); err == nil {
				t.Fatal("newARMProxyTransport() error = nil, want error")
			}
		})
	}
}

func TestStaticARMProxyCredential(t *testing.T) {
	t.Parallel()

	token, err := (staticARMProxyCredential{}).GetToken(context.Background(), policy.TokenRequestOptions{})
	if err != nil {
		t.Fatalf("GetToken() error = %v", err)
	}
	if token.Token != fakeARMProxyBearerToken {
		t.Fatalf("Token = %q, want %q", token.Token, fakeARMProxyBearerToken)
	}
	if token.ExpiresOn.IsZero() {
		t.Fatal("ExpiresOn is zero")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}
