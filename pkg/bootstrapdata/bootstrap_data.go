package bootstrapdata

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
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
	maxThrottleRetries          = 30_000
	initialThrottleRetryDelay   = time.Second
	initialThrottleRetryJitter  = 5 * time.Minute
	maxThrottleRetryJitter      = time.Hour
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
// listBootstrapData. Runtime callers should use the typed fields.
type Data struct {
	BootstrapToken string
	ClusterFQDN    string
	CACertData     string
	raw            json.RawMessage
}

type dependencies struct {
	credential   func(Options, azcore.ClientOptions) (azcore.TokenCredential, error)
	transport    policy.Transporter
	retryOptions *policy.RetryOptions
	retryJitter  func(time.Duration) time.Duration
}

func defaultDependencies() dependencies {
	return dependencies{
		credential: newCredential,
		transport: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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
	retryCtx, cancel := context.WithTimeout(ctx, bootstrapDataRetryTimeout)
	defer cancel()
	endpoint := strings.TrimRight(options.ResourceManagerEndpoint, "/")
	audience := strings.TrimRight(options.ResourceManagerAudience, "/")
	if audience == "" {
		audience = endpoint
	}
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
	clusterID, err := arm.ParseResourceID(options.ClusterResourceID)
	if err != nil {
		return nil, fmt.Errorf("parse cluster resource ID: %w", err)
	}
	retryOptions := bootstrapDataRetryOptions()
	if deps.retryOptions != nil {
		retryOptions = *deps.retryOptions
	}
	transport := deps.transport
	if transport == nil {
		transport = http.DefaultClient
	}
	client, err := armcontainerservice.NewAgentPoolsClient(clusterID.SubscriptionID, credential, &arm.ClientOptions{
		ClientOptions: policy.ClientOptions{
			APIVersion:       options.APIVersion,
			Cloud:            clientOptions.Cloud,
			Retry:            retryOptions,
			Transport:        limitedResponseTransport{inner: transport},
			PerRetryPolicies: []policy.Policy{&retryAfterJitterPolicy{jitter: deps.retryJitter}},
		},
		DisableRPRegistration: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create AgentPools client: %w", err)
	}
	var rawResponse *http.Response
	response, err := client.ListBootstrapData(
		policy.WithCaptureResponse(retryCtx, &rawResponse),
		clusterID.ResourceGroupName,
		clusterID.Name,
		options.AgentPoolName,
		armcontainerservice.ListBootstrapDataRequest{},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("list bootstrap data: %w", err)
	}
	if rawResponse == nil {
		return nil, fmt.Errorf("list bootstrap data returned no HTTP response")
	}
	raw, err := runtime.Payload(rawResponse)
	if err != nil {
		return nil, fmt.Errorf("read bootstrap data: %w", err)
	}
	if int64(len(raw)) > maxResponseBytes {
		return nil, fmt.Errorf("bootstrap data exceeds %d bytes", maxResponseBytes)
	}
	responseData := response.PoolBootstrapData
	bootstrapToken := ""
	if responseData.Azure != nil && responseData.Azure.BootstrapToken != nil && responseData.Azure.BootstrapToken.Token != nil {
		bootstrapToken = *responseData.Azure.BootstrapToken.Token
	}
	if bootstrapToken == "" {
		return nil, fmt.Errorf("bootstrap-data response did not contain a bootstrap token")
	}
	if !config.BootstrapTokenPattern.MatchString(bootstrapToken) {
		return nil, fmt.Errorf("bootstrap-data response contained an invalid bootstrap token")
	}
	clusterFQDN := ""
	caCertData := ""
	if responseData.Node != nil && responseData.Node.Kubelet != nil {
		if responseData.Node.Kubelet.ClusterFQDN != nil {
			clusterFQDN = *responseData.Node.Kubelet.ClusterFQDN
		}
		if responseData.Node.Kubelet.CaCertData != nil {
			caCertData = *responseData.Node.Kubelet.CaCertData
		}
	}
	return &Data{
		BootstrapToken: bootstrapToken,
		ClusterFQDN:    clusterFQDN,
		CACertData:     caCertData,
		raw:            append(json.RawMessage(nil), raw...),
	}, nil
}

func bootstrapDataRetryOptions() policy.RetryOptions {
	return policy.RetryOptions{
		MaxRetries:    maxThrottleRetries,
		TryTimeout:    bootstrapDataAttemptTimeout,
		RetryDelay:    initialThrottleRetryDelay,
		MaxRetryDelay: bootstrapDataRetryTimeout,
		ShouldRetry: func(response *http.Response, err error) bool {
			return err == nil && response != nil && response.StatusCode == http.StatusTooManyRequests
		},
	}
}

type retryAfterJitterPolicy struct {
	attempt int
	jitter  func(time.Duration) time.Duration
}

func (p *retryAfterJitterPolicy) Do(request *policy.Request) (*http.Response, error) {
	response, err := request.Next()
	if err == nil && response != nil && response.StatusCode == http.StatusTooManyRequests {
		if addRetryAfterJitter(response, p.attempt, p.jitter) {
			p.attempt++
		}
	}
	return response, err
}

func addRetryAfterJitter(response *http.Response, attempt int, jitter func(time.Duration) time.Duration) bool {
	retryAfterSeconds, err := strconv.ParseUint(response.Header.Get("Retry-After"), 10, 31)
	if err != nil || retryAfterSeconds == 0 {
		return false
	}
	if jitter == nil {
		jitter = randomRetryJitter
	}
	window := min(initialThrottleRetryJitter*time.Duration(1<<min(attempt, 4)), maxThrottleRetryJitter)
	delay := time.Duration(retryAfterSeconds)*time.Second + jitter(window)
	response.Header.Set("Retry-After-Ms", strconv.FormatInt(delay.Milliseconds(), 10))
	return true
}

func randomRetryJitter(maxDelay time.Duration) time.Duration {
	jitter, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(maxDelay)+1))
	if err != nil {
		return maxDelay / 2
	}
	return time.Duration(jitter.Int64())
}

type limitedResponseTransport struct {
	inner policy.Transporter
}

func (t limitedResponseTransport) Do(request *http.Request) (*http.Response, error) {
	response, err := t.inner.Do(request)
	if err != nil || response == nil || response.Body == nil {
		return response, err
	}
	response.Body = struct {
		io.Reader
		io.Closer
	}{
		Reader: io.LimitReader(response.Body, maxResponseBytes+1),
		Closer: response.Body,
	}
	return response, nil
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
