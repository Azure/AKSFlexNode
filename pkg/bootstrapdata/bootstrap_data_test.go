package bootstrapdata

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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

func TestFetchRetriesTooManyRequests(t *testing.T) {
	t.Parallel()

	const responseBody = `{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`
	attempts := 0
	throttledBodyClosed := false
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       &trackingBody{Reader: strings.NewReader("throttled"), closed: &throttledBodyClosed},
				Header:     http.Header{"Retry-After": []string{"2"}},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(responseBody)),
			Header:     make(http.Header),
		}, nil
	})}
	var delays []time.Duration
	got, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential: staticCredentialFactory,
		httpClient: client,
		jitter:     func(time.Duration) time.Duration { return 500 * time.Millisecond },
		sleep: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
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
	if !throttledBodyClosed {
		t.Fatal("throttled response body was not closed")
	}
	if len(delays) != 1 || delays[0] != 2500*time.Millisecond {
		t.Fatalf("retry delays = %v, want [2.5s]", delays)
	}
}

func TestFetchRefreshesARMTokenBeforeRetry(t *testing.T) {
	t.Parallel()

	credential := &countingCredential{}
	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		wantToken := "Bearer token-" + strconv.Itoa(attempts)
		if got := request.Header.Get("Authorization"); got != wantToken {
			t.Fatalf("Authorization = %q, want %q", got, wantToken)
		}
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Body:       io.NopCloser(strings.NewReader("throttled")),
				Header:     http.Header{"Retry-After": []string{"1"}},
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(`{"azure":{"bootstrapToken":{"token":"abcdef.0123456789abcdef"}}}`)),
			Header:     make(http.Header),
		}, nil
	})}
	_, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential: func(Options, azcore.ClientOptions) (azcore.TokenCredential, error) { return credential, nil },
		httpClient: client,
		sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("fetch() error = %v", err)
	}
	if credential.calls != 2 {
		t.Fatalf("GetToken calls = %d, want 2", credential.calls)
	}
}

func TestFetchStopsAfterThrottleRetriesExhausted(t *testing.T) {
	t.Parallel()

	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader("throttled")),
			Header:     http.Header{"Retry-After": []string{"0"}},
		}, nil
	})}
	waits := 0
	_, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential:         staticCredentialFactory,
		httpClient:         client,
		maxThrottleRetries: 3,
		sleep: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	})
	if err == nil || err.Error() != "fetch bootstrap data returned HTTP status 429" {
		t.Fatalf("fetch() error = %v, want final HTTP 429", err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4", attempts)
	}
	if waits != 3 {
		t.Fatalf("waits = %d, want 3", waits)
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
		credential: staticCredentialFactory,
		httpClient: client,
	})
	if err == nil || err.Error() != "fetch bootstrap data returned HTTP status 503" {
		t.Fatalf("fetch() error = %v, want HTTP 503", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestFetchStopsRetryingWhenContextIsCancelled(t *testing.T) {
	t.Parallel()

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader("throttled")),
			Header:     http.Header{"Retry-After": []string{"1"}},
		}, nil
	})}
	ctx, cancel := context.WithCancel(t.Context())
	_, err := fetch(ctx, validTestOptions(), dependencies{
		credential: staticCredentialFactory,
		httpClient: client,
		sleep: func(ctx context.Context, _ time.Duration) error {
			cancel()
			return ctx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("fetch() error = %v, want context cancellation", err)
	}
}

func TestFetchDoesNotRetryBeyondDeadline(t *testing.T) {
	t.Parallel()

	attempts := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Body:       io.NopCloser(strings.NewReader("throttled")),
			Header:     http.Header{"Retry-After": []string{"46800"}},
		}, nil
	})}
	_, err := fetch(t.Context(), validTestOptions(), dependencies{
		credential: staticCredentialFactory,
		httpClient: client,
		sleep: func(context.Context, time.Duration) error {
			t.Fatal("unexpected retry wait")
			return nil
		},
	})
	if err == nil || err.Error() != "fetch bootstrap data returned HTTP status 429" {
		t.Fatalf("fetch() error = %v, want final HTTP 429", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestParseRetryAfter(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{name: "seconds", value: "3", want: 3 * time.Second, ok: true},
		{name: "HTTP date", value: now.Add(5 * time.Second).Format(http.TimeFormat), want: 5 * time.Second, ok: true},
		{name: "past HTTP date", value: now.Add(-time.Second).Format(http.TimeFormat), want: 0, ok: true},
		{name: "negative seconds", value: "-1", ok: false},
		{name: "invalid", value: "later", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseRetryAfter(tt.value, now)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("parseRetryAfter(%q) = (%s, %t), want (%s, %t)", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestThrottleRetryDelayUsesServerMinimum(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	delay := throttleRetryDelay("30", 0, now, func(time.Duration) time.Duration { return 500 * time.Millisecond })
	if delay != 30500*time.Millisecond {
		t.Fatalf("throttleRetryDelay() = %s, want 30.5s", delay)
	}
}

func TestThrottleRetryDelayUsesExponentialFullJitter(t *testing.T) {
	t.Parallel()

	var bounds []time.Duration
	for retry := range 14 {
		_ = throttleRetryDelay("", retry, time.Time{}, func(bound time.Duration) time.Duration {
			bounds = append(bounds, bound)
			return 0
		})
	}
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, 512 * time.Second, 1024 * time.Second, 2048 * time.Second,
		time.Hour, time.Hour,
	}
	for i := range want {
		if bounds[i] != want[i] {
			t.Fatalf("retry %d jitter bound = %s, want %s", i, bounds[i], want[i])
		}
	}
}

func TestThrottleRetryBudgetCoversThirtyThousandNodes(t *testing.T) {
	t.Parallel()

	const nodeCount = 30_000
	minimumDrainTime := nodeCount * time.Second
	if bootstrapDataRetryTimeout <= minimumDrainTime {
		t.Fatalf("retry timeout = %s, want more than %s", bootstrapDataRetryTimeout, minimumDrainTime)
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

type trackingBody struct {
	io.Reader
	closed *bool
}

type countingCredential struct {
	calls int
}

func (c *countingCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls++
	return azcore.AccessToken{Token: "token-" + strconv.Itoa(c.calls), ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func (b *trackingBody) Close() error {
	*b.closed = true
	return nil
}
