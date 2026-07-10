package aksmachine

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	//nolint:gosec // Static dev-test token sent only to the configured ARM proxy, never to Azure.
	fakeARMProxyBearerToken    = "aks-flex-node-e2e"
	fakeARMProxyTokenExpiresIn = time.Hour
)

// newARMProxyClient returns an ARM Machine API client redirected to a dev-test proxy.
func newARMProxyClient(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	machineID, err := machineResourceIDFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	transport, err := newARMProxyTransport(cfg.Agent.MachineClient.EndpointURL, nil)
	if err != nil {
		return nil, fmt.Errorf("configure ARM proxy override: %w", err)
	}
	logger.Warn("using dev-test ARM proxy URL override")
	client, err := armcontainerservice.NewMachinesClient(
		machineID.SubscriptionID,
		staticARMProxyCredential{},
		&arm.ClientOptions{
			ClientOptions: policy.ClientOptions{
				Transport: transport,
			},
		})
	if err != nil {
		return nil, fmt.Errorf("create proxied machines client: %w", err)
	}
	return &armMachineClient{
		machineID: machineID,
		client:    client,
		logger:    logger,
	}, nil
}

type staticARMProxyCredential struct{}

func (staticARMProxyCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{
		Token:     fakeARMProxyBearerToken,
		ExpiresOn: time.Now().Add(fakeARMProxyTokenExpiresIn),
	}, nil
}

type armProxyTransport struct {
	proxy *url.URL
	next  policy.Transporter
}

func newARMProxyTransport(proxyURL string, next policy.Transporter) (*armProxyTransport, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("proxy URL must be absolute")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("proxy URL scheme must be http or https")
	}
	if next == nil {
		next = roundTripperTransport{next: http.DefaultTransport}
	}
	return &armProxyTransport{proxy: parsed, next: next}, nil
}

type roundTripperTransport struct {
	next http.RoundTripper
}

func (t roundTripperTransport) Do(req *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(req)
}

func (t *armProxyTransport) Do(req *http.Request) (*http.Response, error) {
	proxied := req.Clone(req.Context())
	proxied.URL = cloneURL(req.URL)
	proxied.URL.Scheme = t.proxy.Scheme
	proxied.URL.Host = t.proxy.Host
	proxied.URL.Path = joinURLPath(t.proxy.Path, req.URL.Path)
	proxied.URL.RawPath = ""
	proxied.URL.RawQuery = mergeRawQuery(t.proxy.RawQuery, req.URL.RawQuery)
	proxied.Host = t.proxy.Host
	return t.next.Do(proxied)
}

func cloneURL(u *url.URL) *url.URL {
	cloned := *u
	return &cloned
}

func joinURLPath(prefix, path string) string {
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		if path == "" {
			return "/"
		}
		return path
	}
	if path == "" || path == "/" {
		return prefix
	}
	return prefix + "/" + strings.TrimLeft(path, "/")
}

func mergeRawQuery(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "&" + second
	}
}
