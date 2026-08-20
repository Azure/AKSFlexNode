package bootstrapdata

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/google/renameio/v2"

	"github.com/Azure/AKSFlexNode/pkg/azclient"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

const (
	DefaultAPIVersion              = "2026-05-02-preview"
	DefaultResourceManagerEndpoint = "https://management.azure.com"
	DefaultAuthorityHost           = "https://login.microsoftonline.com"
	maxResponseBytes               = int64(16 << 20)
	// The RP bucket refills at one request per second. Twelve hours lets a
	// 30,000-node scale-out drain with headroom while keeping retries bounded.
	maxThrottleRetries          = 1_000
	initialThrottleRetryDelay   = time.Second
	maxThrottleBackoffDelay     = time.Hour
	bootstrapDataAttemptTimeout = 2 * time.Minute
	bootstrapDataRetryTimeout   = 12 * time.Hour
)

var (
	apiVersionPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?$`)
	poolNamePattern   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
)

type Options struct {
	ClusterResourceID       string
	AgentPoolName           string
	AuthMode                string
	MSIClientID             string
	SPTenantID              string
	SPClientID              string
	SPClientSecret          string
	SPClientSecretFile      string
	SPClientCertificateFile string
	SPClientCredentialFile  string
	ResourceManagerEndpoint string
	ResourceManagerAudience string
	AuthorityHost           string
	APIVersion              string
	OutputPath              string
}

// OptionsFromConfig converts validated runtime configuration into options for
// an in-memory listBootstrapData request.
func OptionsFromConfig(cfg *config.Config) (Options, error) {
	if cfg == nil || cfg.Azure.TargetCluster == nil {
		return Options{}, fmt.Errorf("target AKS cluster is not configured")
	}
	environment := azclient.ResourceManagerEnvironmentFromConfig(cfg)
	options := Options{
		ClusterResourceID:       cfg.Azure.TargetCluster.ResourceID,
		AgentPoolName:           cfg.Azure.TargetAgentPoolName,
		ResourceManagerEndpoint: environment.Endpoint,
		ResourceManagerAudience: environment.Audience,
		AuthorityHost:           environment.AuthorityHost,
		APIVersion:              DefaultAPIVersion,
	}

	switch {
	case cfg.IsARCEnabled():
		options.AuthMode = "arc"
	case cfg.IsMIConfigured():
		options.AuthMode = "managed-identity"
		options.MSIClientID = cfg.Azure.ManagedIdentity.ClientID
	case cfg.IsSPConfigured():
		options.AuthMode = "service-principal"
		options.SPTenantID = cfg.Azure.ServicePrincipal.TenantID
		options.SPClientID = cfg.Azure.ServicePrincipal.ClientID
		if cfg.Azure.ServicePrincipal.ClientSecretFile != "" {
			// Config validation leaves ClientSecretFile populated only when it
			// contains a certificate. Secret files are loaded into ClientSecret.
			options.SPClientCertificateFile = cfg.Azure.ServicePrincipal.ClientSecretFile
		} else {
			options.SPClientSecret = cfg.Azure.ServicePrincipal.ClientSecret
		}
	default:
		return Options{}, fmt.Errorf("bootstrap-data refresh requires Arc, managed identity, or service-principal authentication")
	}

	return options, nil
}

// Data contains the short-lived Kubernetes join credentials returned by
// listBootstrapData. The raw response is retained only so the bootstrap CLI can
// write the complete RP response; runtime callers should use the typed fields.
type Data struct {
	BootstrapToken string
	ClusterFQDN    string
	CACertData     string
	raw            map[string]any
}

type dependencies struct {
	credential         func(Options, azcore.ClientOptions) (azcore.TokenCredential, error)
	httpClient         *http.Client
	sleep              func(context.Context, time.Duration) error
	jitter             func(time.Duration) time.Duration
	maxThrottleRetries int
	retryTimeout       time.Duration
}

func defaultDependencies() dependencies {
	return dependencies{
		credential: newCredential,
		httpClient: &http.Client{
			Timeout: bootstrapDataAttemptTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("bootstrap-data redirects are not allowed")
			},
		},
		sleep:              sleepWithContext,
		jitter:             fullJitter,
		maxThrottleRetries: maxThrottleRetries,
		retryTimeout:       bootstrapDataRetryTimeout,
	}
}

// Fetch obtains fresh bootstrap data without persisting the sensitive response.
func Fetch(ctx context.Context, options Options) (*Data, error) {
	return fetch(ctx, options, defaultDependencies())
}

func FetchAndWrite(ctx context.Context, options Options) error {
	return fetchAndWrite(ctx, options, defaultDependencies())
}

func fetchAndWrite(ctx context.Context, options Options, deps dependencies) error {
	if strings.TrimSpace(options.OutputPath) == "" {
		return fmt.Errorf("output path is required")
	}
	result, err := fetch(ctx, options, deps)
	if err != nil {
		return err
	}
	formatted, err := json.MarshalIndent(result.raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal bootstrap data: %w", err)
	}
	formatted = append(formatted, '\n')
	if err := os.MkdirAll(filepath.Dir(options.OutputPath), 0o750); err != nil {
		return fmt.Errorf("create bootstrap-data output directory: %w", err)
	}
	if err := renameio.WriteFile(options.OutputPath, formatted, 0o600); err != nil {
		return fmt.Errorf("atomically write bootstrap data: %w", err)
	}
	return os.Chmod(options.OutputPath, 0o600)
}

func fetch(ctx context.Context, options Options, deps dependencies) (*Data, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(options.ResourceManagerEndpoint, "/")
	audience := strings.TrimRight(options.ResourceManagerAudience, "/")
	if audience == "" {
		audience = endpoint
	}
	retryTimeout := deps.retryTimeout
	if retryTimeout <= 0 {
		retryTimeout = bootstrapDataRetryTimeout
	}
	retryCtx, cancel := context.WithTimeout(ctx, retryTimeout)
	defer cancel()
	clientOptions := azcore.ClientOptions{Cloud: cloud.Configuration{
		ActiveDirectoryAuthorityHost: options.AuthorityHost,
		Services: map[cloud.ServiceName]cloud.ServiceConfiguration{
			cloud.ResourceManager: {Endpoint: endpoint, Audience: audience},
		},
	}}
	credential, err := deps.credential(options, clientOptions)
	if err != nil {
		return nil, err
	}
	token, err := credential.GetToken(retryCtx, policy.TokenRequestOptions{Scopes: []string{audience + "/.default"}})
	if err != nil {
		return nil, fmt.Errorf("acquire ARM token: %w", err)
	}
	requestURL := endpoint + options.ClusterResourceID + "/agentPools/" + options.AgentPoolName +
		"/listBootstrapData?api-version=" + options.APIVersion
	request, err := http.NewRequestWithContext(retryCtx, http.MethodPost, requestURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create bootstrap-data request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := doBootstrapDataRequest(retryCtx, request, credential, audience, deps)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap data: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("fetch bootstrap data returned HTTP status %d", response.StatusCode)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read bootstrap data: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close bootstrap-data response: %w", closeErr)
	}
	if int64(len(data)) > maxResponseBytes {
		return nil, fmt.Errorf("bootstrap data exceeds %d bytes", maxResponseBytes)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse bootstrap data: %w", err)
	}
	var responseData struct {
		Azure struct {
			BootstrapToken struct {
				Token string `json:"token"`
			} `json:"bootstrapToken"`
		} `json:"azure"`
		Node struct {
			Kubelet struct {
				ClusterFQDN string `json:"clusterFQDN"`
				CACertData  string `json:"caCertData"`
			} `json:"kubelet"`
		} `json:"node"`
	}
	if err := json.Unmarshal(data, &responseData); err != nil {
		return nil, fmt.Errorf("parse typed bootstrap data: %w", err)
	}
	if responseData.Azure.BootstrapToken.Token == "" {
		return nil, fmt.Errorf("bootstrap-data response did not contain a bootstrap token")
	}
	if !config.BootstrapTokenPattern.MatchString(responseData.Azure.BootstrapToken.Token) {
		return nil, fmt.Errorf("bootstrap-data response contained an invalid bootstrap token")
	}
	return &Data{
		BootstrapToken: responseData.Azure.BootstrapToken.Token,
		ClusterFQDN:    responseData.Node.Kubelet.ClusterFQDN,
		CACertData:     responseData.Node.Kubelet.CACertData,
		raw:            raw,
	}, nil
}

func doBootstrapDataRequest(
	ctx context.Context,
	request *http.Request,
	credential azcore.TokenCredential,
	audience string,
	deps dependencies,
) (*http.Response, error) {
	maxRetries := deps.maxThrottleRetries
	if maxRetries <= 0 {
		maxRetries = maxThrottleRetries
	}
	for retry := 0; ; retry++ {
		if retry > 0 {
			token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{audience + "/.default"}})
			if err != nil {
				return nil, fmt.Errorf("refresh ARM token: %w", err)
			}
			request.Header.Set("Authorization", "Bearer "+token.Token)
		}
		response, err := deps.httpClient.Do(request.Clone(ctx))
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusTooManyRequests || retry == maxRetries {
			return response, nil
		}

		delay := throttleRetryDelay(response.Header.Get("Retry-After"), retry, time.Now(), deps.jitter)
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= delay {
			return response, nil
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		sleepFn := deps.sleep
		if sleepFn == nil {
			sleepFn = sleepWithContext
		}
		if err := sleepFn(ctx, delay); err != nil {
			return nil, fmt.Errorf("wait to retry bootstrap data after HTTP 429: %w", err)
		}
	}
}

func throttleRetryDelay(retryAfter string, retry int, now time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	backoff := min(initialThrottleRetryDelay*time.Duration(1<<min(retry, 12)), maxThrottleBackoffDelay)
	if jitter == nil {
		jitter = fullJitter
	}
	delay := jitter(backoff)
	if retryAfter != "" {
		retryAfterDelay, ok := parseRetryAfter(retryAfter, now)
		if ok {
			if delay <= time.Duration(math.MaxInt64)-retryAfterDelay {
				delay += retryAfterDelay
			} else {
				delay = time.Duration(math.MaxInt64)
			}
		}
	}
	return delay
}

func fullJitter(maxDelay time.Duration) time.Duration {
	if maxDelay <= 0 {
		return 0
	}
	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxDelay)+1))
	if err != nil {
		return maxDelay
	}
	return time.Duration(jitter.Int64())
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0, false
		}
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64), true
		}
		return time.Duration(seconds) * time.Second, true
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(retryAt.Sub(now), 0), true
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func validateOptions(options Options) error {
	endpoint, err := url.Parse(options.ResourceManagerEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil {
		return fmt.Errorf("resource manager endpoint must be an absolute HTTPS URL")
	}
	if options.ResourceManagerAudience != "" {
		audience, err := url.Parse(options.ResourceManagerAudience)
		if err != nil || audience.Scheme != "https" || audience.Host == "" || audience.User != nil {
			return fmt.Errorf("resource manager audience must be an absolute HTTPS URL")
		}
	}
	authority, err := url.Parse(options.AuthorityHost)
	if err != nil || authority.Scheme != "https" || authority.Host == "" || authority.User != nil {
		return fmt.Errorf("authority host must be an absolute HTTPS URL")
	}
	clusterID, err := arm.ParseResourceID(options.ClusterResourceID)
	if err != nil {
		return fmt.Errorf("parse cluster resource ID: %w", err)
	}
	if !strings.EqualFold(clusterID.ResourceType.String(), "Microsoft.ContainerService/managedClusters") {
		return fmt.Errorf("resource ID is not an AKS managed cluster")
	}
	if !poolNamePattern.MatchString(options.AgentPoolName) {
		return fmt.Errorf("invalid agent pool name %q", options.AgentPoolName)
	}
	if !apiVersionPattern.MatchString(options.APIVersion) {
		return fmt.Errorf("invalid API version %q", options.APIVersion)
	}
	return nil
}

func newCredential(options Options, clientOptions azcore.ClientOptions) (azcore.TokenCredential, error) {
	switch strings.ToLower(strings.TrimSpace(options.AuthMode)) {
	case "arc":
		if options.MSIClientID != "" {
			return nil, fmt.Errorf("arc authentication does not support a managed identity client ID")
		}
		return azidentity.NewManagedIdentityCredential(
			&azidentity.ManagedIdentityCredentialOptions{ClientOptions: clientOptions},
		)
	case "msi", "managed-identity":
		credentialOptions := &azidentity.ManagedIdentityCredentialOptions{ClientOptions: clientOptions}
		if options.MSIClientID != "" {
			credentialOptions.ID = azidentity.ClientID(options.MSIClientID)
		}
		credential, err := azidentity.NewManagedIdentityCredential(credentialOptions)
		if err != nil {
			return nil, fmt.Errorf("create managed identity credential: %w", err)
		}
		return credential, nil
	case "sp", "service-principal":
		if options.SPTenantID == "" || options.SPClientID == "" {
			return nil, fmt.Errorf("service-principal auth requires tenant ID and client ID")
		}
		return newServicePrincipalCredential(options, clientOptions)
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", options.AuthMode)
	}
}

func newServicePrincipalCredential(options Options, clientOptions azcore.ClientOptions) (azcore.TokenCredential, error) {
	credentialCount := 0
	if options.SPClientSecret != "" {
		credentialCount++
	}
	if options.SPClientSecretFile != "" {
		credentialCount++
	}
	if options.SPClientCertificateFile != "" {
		credentialCount++
	}
	if options.SPClientCredentialFile != "" {
		credentialCount++
	}
	if credentialCount != 1 {
		return nil, fmt.Errorf("service-principal auth requires exactly one secret, secret file, certificate file, or auto-detected credential file")
	}
	if options.SPClientCertificateFile != "" {
		return newCertificateCredential(options.SPTenantID, options.SPClientID, options.SPClientCertificateFile, clientOptions)
	}
	if options.SPClientCredentialFile != "" {
		certificate, err := credentialFileLooksLikeCertificate(options.SPClientCredentialFile)
		if err != nil {
			return nil, err
		}
		if certificate {
			return newCertificateCredential(options.SPTenantID, options.SPClientID, options.SPClientCredentialFile, clientOptions)
		}
		options.SPClientSecretFile = options.SPClientCredentialFile
	}
	secret := options.SPClientSecret
	if secret == "" {
		data, err := config.LoadServicePrincipalCredentialFile(options.SPClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("load service-principal secret: %w", err)
		}
		secret = strings.TrimRight(string(data), "\r\n")
	}
	if secret == "" {
		return nil, fmt.Errorf("service-principal client secret is empty")
	}
	credential, err := azidentity.NewClientSecretCredential(options.SPTenantID, options.SPClientID, secret,
		&azidentity.ClientSecretCredentialOptions{ClientOptions: clientOptions})
	if err != nil {
		return nil, fmt.Errorf("create client-secret credential: %w", err)
	}
	return credential, nil
}

func newCertificateCredential(tenantID, clientID, path string, clientOptions azcore.ClientOptions) (azcore.TokenCredential, error) {
	if err := config.ValidateServicePrincipalCertificateFile(path); err != nil {
		return nil, fmt.Errorf("validate service-principal certificate: %w", err)
	}
	servicePrincipal := config.ServicePrincipalConfig{ClientSecretFile: path}
	certificates, privateKey, err := servicePrincipal.LoadClientCertificate()
	if err != nil {
		return nil, fmt.Errorf("load service-principal certificate: %w", err)
	}
	credential, err := azidentity.NewClientCertificateCredential(tenantID, clientID, certificates, privateKey,
		clientCertificateCredentialOptions(clientOptions))
	if err != nil {
		return nil, fmt.Errorf("create client-certificate credential: %w", err)
	}
	return credential, nil
}

func clientCertificateCredentialOptions(clientOptions azcore.ClientOptions) *azidentity.ClientCertificateCredentialOptions {
	return &azidentity.ClientCertificateCredentialOptions{
		ClientOptions:        clientOptions,
		SendCertificateChain: true,
	}
}

func credentialFileLooksLikeCertificate(path string) (bool, error) {
	data, err := config.LoadServicePrincipalCredentialFile(path)
	if err != nil {
		return false, fmt.Errorf("load service-principal credential file: %w", err)
	}
	return bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) ||
		bytes.Contains(data, []byte("-----BEGIN PRIVATE KEY-----")) ||
		bytes.Contains(data, []byte("-----BEGIN RSA PRIVATE KEY-----")) || !utf8.Valid(data), nil
}
