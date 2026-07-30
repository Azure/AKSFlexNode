package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/google/renameio/v2"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const maxBootstrapDataBytes = int64(16 << 20)

var (
	poolNamePattern   = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	apiVersionPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?$`)
)

type object = map[string]any

type dependencies struct {
	credential func(object, Options) (azcore.TokenCredential, error)
	httpClient *http.Client
}

func defaultDependencies() dependencies {
	return dependencies{
		credential: bootstrapCredential,
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("bootstrap-data redirects are not allowed")
			},
		},
	}
}

func BuildConfig(ctx context.Context, options Options) ([]byte, error) {
	return buildConfig(ctx, options, defaultDependencies())
}

func buildConfig(ctx context.Context, options Options, deps dependencies) ([]byte, error) {
	if err := validateOptions(options); err != nil {
		return nil, err
	}
	base, err := loadBaseObject(options.BaseConfigPath)
	if err != nil {
		return nil, err
	}
	applyTargetOverrides(base, options)
	if options.FetchBootstrapData {
		data, err := fetchBootstrapData(ctx, base, options, deps)
		if err != nil {
			return nil, err
		}
		deepMerge(base, data)
	}
	for index, raw := range options.ConfigOverrides {
		override, err := decodeObject([]byte(raw))
		if err != nil {
			return nil, fmt.Errorf("parse config override %d: %w", index+1, err)
		}
		deepMerge(base, override)
	}
	applyTargetOverrides(base, options)
	applyBootstrapSourceOverrides(base, options)
	normalizeOfflineVersions(base)
	if err := applyNodeName(base); err != nil {
		return nil, err
	}
	if err := applyAuth(base, options); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal rendered config: %w", err)
	}
	data = append(data, '\n')
	if err := validateConfigData(data); err != nil {
		return nil, err
	}
	return data, nil
}

func WriteConfig(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := renameio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("atomically write config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set config permissions: %w", err)
	}
	return nil
}

func loadBaseObject(path string) (object, error) {
	if strings.TrimSpace(path) == "" {
		return object{}, nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read base config: %w", err)
	}
	base, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("parse base config: %w", err)
	}
	return base, nil
}

func decodeObject(data []byte) (object, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values are not allowed")
		}
		return nil, err
	}
	result, ok := value.(object)
	if !ok {
		return nil, fmt.Errorf("JSON value must be an object")
	}
	return result, nil
}

func deepMerge(destination, source object) {
	for key, value := range source {
		if sourceObject, ok := value.(object); ok {
			if destinationObject, ok := destination[key].(object); ok {
				deepMerge(destinationObject, sourceObject)
				continue
			}
		}
		destination[key] = value
	}
}

func ensureObject(parent object, key string) object {
	if value, ok := parent[key].(object); ok {
		return value
	}
	value := object{}
	parent[key] = value
	return value
}

func applyTargetOverrides(root object, options Options) {
	azure := ensureObject(root, "azure")
	if options.ClusterResourceID != "" {
		ensureObject(azure, "targetCluster")["resourceId"] = options.ClusterResourceID
	}
	if options.AgentPoolName != "" {
		azure["targetAgentPoolName"] = options.AgentPoolName
	}
	if options.ResourceManagerEndpoint != "" {
		azure["resourceManagerEndpoint"] = options.ResourceManagerEndpoint
	}
}

func applyBootstrapSourceOverrides(root object, options Options) {
	bootstrap := ensureObject(root, "bootstrap")
	if options.BootstrapOCIImage != "" {
		bootstrap["ociImage"] = options.BootstrapOCIImage
	}
	if options.BootstrapOfflineArtifactsSource != "" {
		ensureObject(bootstrap, "offlineArtifacts")["source"] = options.BootstrapOfflineArtifactsSource
	}
}

func normalizeOfflineVersions(root object) {
	bootstrap, _ := root["bootstrap"].(object)
	offline, _ := bootstrap["offlineArtifacts"].(object)
	source, _ := offline["source"].(string)
	if strings.TrimSpace(source) == "" {
		return
	}
	components := ensureObject(root, "components")
	delete(components, "containerd")
	delete(components, "runc")
	delete(ensureObject(root, "networking"), "cniVersion")
}

func applyNodeName(root object) error {
	agent := ensureObject(root, "agent")
	if name, _ := agent["nodeName"].(string); strings.TrimSpace(name) != "" {
		return nil
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get host name: %w", err)
	}
	agent["nodeName"] = strings.ToLower(strings.TrimSpace(hostname))
	return nil
}

func validateOptions(options Options) error {
	mode := strings.ToLower(strings.TrimSpace(options.AuthMode))
	if mode != "sp" && mode != "service-principal" {
		return nil
	}
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
	if credentialCount != 1 {
		return fmt.Errorf("service-principal auth requires exactly one secret, secret file, or certificate file")
	}
	for label, path := range map[string]string{
		"service-principal secret file":      options.SPClientSecretFile,
		"service-principal certificate file": options.SPClientCertificateFile,
	} {
		if path != "" && !filepath.IsAbs(path) {
			return fmt.Errorf("%s path must be absolute", label)
		}
	}
	return nil
}

func applyAuth(root object, options Options) error {
	mode := strings.ToLower(strings.TrimSpace(options.AuthMode))
	if mode == "" {
		return nil
	}
	azure := ensureObject(root, "azure")
	ensureObject(azure, "arc")["enabled"] = false
	switch mode {
	case "msi", "managed-identity":
		delete(azure, "servicePrincipal")
		identity := object{}
		if options.MSIClientID != "" {
			identity["clientId"] = options.MSIClientID
		}
		azure["managedIdentity"] = identity
	case "sp", "service-principal":
		if options.SPClientID == "" {
			return fmt.Errorf("service-principal auth requires client ID")
		}
		delete(azure, "managedIdentity")
		tenantID := options.SPTenantID
		if tenantID == "" {
			tenantID, _ = azure["tenantId"].(string)
		}
		servicePrincipal := object{"tenantId": tenantID, "clientId": options.SPClientID}
		switch {
		case options.SPClientCertificateFile != "":
			servicePrincipal["clientSecretFile"] = options.SPClientCertificateFile
		case options.SPClientSecretFile != "":
			servicePrincipal["clientSecretFile"] = options.SPClientSecretFile
		default:
			servicePrincipal["clientSecret"] = options.SPClientSecret
		}
		azure["servicePrincipal"] = servicePrincipal
	default:
		return fmt.Errorf("unsupported auth mode %q", options.AuthMode)
	}
	return nil
}

func fetchBootstrapData(ctx context.Context, root object, options Options, deps dependencies) (object, error) {
	azure := ensureObject(root, "azure")
	endpoint := options.ResourceManagerEndpoint
	if endpoint == "" {
		endpoint, _ = azure["resourceManagerEndpoint"].(string)
	}
	if endpoint == "" {
		endpoint = defaultResourceManagerEndpoint
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" || parsedEndpoint.Host == "" {
		return nil, fmt.Errorf("resource manager endpoint must be an absolute HTTPS URL")
	}
	resourceID := options.ClusterResourceID
	if resourceID == "" {
		target, _ := azure["targetCluster"].(object)
		resourceID, _ = target["resourceId"].(string)
	}
	poolName := options.AgentPoolName
	if poolName == "" {
		poolName, _ = azure["targetAgentPoolName"].(string)
	}
	if !poolNamePattern.MatchString(poolName) {
		return nil, fmt.Errorf("invalid agent pool name %q", poolName)
	}
	if !apiVersionPattern.MatchString(options.BootstrapDataAPIVersion) {
		return nil, fmt.Errorf("invalid bootstrap-data API version %q", options.BootstrapDataAPIVersion)
	}
	clusterID, err := arm.ParseResourceID(resourceID)
	if err != nil {
		return nil, fmt.Errorf("parse cluster resource ID: %w", err)
	}
	if !strings.EqualFold(clusterID.ResourceType.String(), "Microsoft.ContainerService/managedClusters") {
		return nil, fmt.Errorf("resource ID is not an AKS managed cluster")
	}
	credential, err := deps.credential(root, options)
	if err != nil {
		return nil, err
	}
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{strings.TrimRight(endpoint, "/") + "/.default"}})
	if err != nil {
		return nil, fmt.Errorf("acquire ARM token: %w", err)
	}
	requestURL := strings.TrimRight(endpoint, "/") + resourceID + "/agentPools/" + poolName +
		"/listBootstrapData?api-version=" + options.BootstrapDataAPIVersion
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create bootstrap-data request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := deps.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap data: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("fetch bootstrap data returned HTTP status %d", response.StatusCode)
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxBootstrapDataBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read bootstrap data: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close bootstrap-data response: %w", closeErr)
	}
	if int64(len(data)) > maxBootstrapDataBytes {
		return nil, fmt.Errorf("bootstrap data exceeds %d bytes", maxBootstrapDataBytes)
	}
	result, err := decodeObject(data)
	if err != nil {
		return nil, fmt.Errorf("parse bootstrap data: %w", err)
	}
	responseAzure, _ := result["azure"].(object)
	tokenConfig, _ := responseAzure["bootstrapToken"].(object)
	bootstrapToken, _ := tokenConfig["token"].(string)
	if bootstrapToken == "" {
		return nil, fmt.Errorf("bootstrap-data response did not contain a bootstrap token")
	}
	return result, nil
}

func bootstrapCredential(root object, options Options) (azcore.TokenCredential, error) {
	mode := strings.ToLower(strings.TrimSpace(options.AuthMode))
	azure := ensureObject(root, "azure")
	if mode == "" {
		switch {
		case azure["managedIdentity"] != nil:
			mode = "msi"
		case azure["servicePrincipal"] != nil:
			mode = "service-principal"
		}
	}
	switch mode {
	case "msi", "managed-identity":
		clientID := options.MSIClientID
		if clientID == "" {
			identity, _ := azure["managedIdentity"].(object)
			clientID, _ = identity["clientId"].(string)
		}
		credentialOptions := &azidentity.ManagedIdentityCredentialOptions{}
		if clientID != "" {
			credentialOptions.ID = azidentity.ClientID(clientID)
		}
		credential, err := azidentity.NewManagedIdentityCredential(credentialOptions)
		if err != nil {
			return nil, fmt.Errorf("create managed identity credential: %w", err)
		}
		return credential, nil
	case "sp", "service-principal":
		servicePrincipal, _ := azure["servicePrincipal"].(object)
		tenantID := options.SPTenantID
		if tenantID == "" {
			tenantID, _ = servicePrincipal["tenantId"].(string)
		}
		if tenantID == "" {
			tenantID, _ = azure["tenantId"].(string)
		}
		clientID := options.SPClientID
		if clientID == "" {
			clientID, _ = servicePrincipal["clientId"].(string)
		}
		if tenantID == "" || clientID == "" {
			return nil, fmt.Errorf("service-principal bootstrap-data fetch requires tenant ID and client ID")
		}
		clientOptions := azcore.ClientOptions{}
		authorityHost := options.AuthorityHost
		if authorityHost == "" {
			authorityHost = defaultAuthorityHost
		}
		clientOptions.Cloud = cloud.Configuration{ActiveDirectoryAuthorityHost: authorityHost}
		certificateFile := options.SPClientCertificateFile
		baseCredentialFile, _ := servicePrincipal["clientSecretFile"].(string)
		if certificateFile == "" && options.SPClientSecretFile == "" && options.SPClientSecret == "" && baseCredentialFile != "" {
			looksLikeCertificate, err := credentialFileLooksLikeCertificate(baseCredentialFile)
			if err != nil {
				return nil, err
			}
			if looksLikeCertificate {
				certificateFile = baseCredentialFile
			}
		}
		if certificateFile != "" {
			if err := config.ValidateServicePrincipalCertificateFile(certificateFile); err != nil {
				return nil, fmt.Errorf("validate service-principal certificate: %w", err)
			}
			sp := config.ServicePrincipalConfig{ClientSecretFile: certificateFile}
			certificates, privateKey, err := sp.LoadClientCertificate()
			if err != nil {
				return nil, fmt.Errorf("load service-principal certificate: %w", err)
			}
			credential, err := azidentity.NewClientCertificateCredential(tenantID, clientID, certificates, privateKey,
				&azidentity.ClientCertificateCredentialOptions{ClientOptions: clientOptions, SendCertificateChain: true})
			if err != nil {
				return nil, fmt.Errorf("create certificate credential: %w", err)
			}
			return credential, nil
		}
		secret := options.SPClientSecret
		secretFile := options.SPClientSecretFile
		if secret == "" && secretFile == "" {
			secret, _ = servicePrincipal["clientSecret"].(string)
			secretFile, _ = servicePrincipal["clientSecretFile"].(string)
		}
		if secret == "" && secretFile != "" {
			data, err := config.LoadServicePrincipalCredentialFile(secretFile)
			if err != nil {
				return nil, fmt.Errorf("load service-principal secret: %w", err)
			}
			secret = strings.TrimRight(string(data), "\r\n")
		}
		if secret == "" {
			return nil, fmt.Errorf("service-principal bootstrap-data fetch requires a secret or certificate")
		}
		credential, err := azidentity.NewClientSecretCredential(tenantID, clientID, secret,
			&azidentity.ClientSecretCredentialOptions{ClientOptions: clientOptions})
		if err != nil {
			return nil, fmt.Errorf("create client-secret credential: %w", err)
		}
		return credential, nil
	default:
		return nil, fmt.Errorf("bootstrap-data fetch requires MSI or service-principal authentication")
	}
}

func credentialFileLooksLikeCertificate(path string) (bool, error) {
	data, err := config.LoadServicePrincipalCredentialFile(path)
	if err != nil {
		return false, fmt.Errorf("load service-principal credential file: %w", err)
	}
	return bytes.Contains(data, []byte("-----BEGIN CERTIFICATE-----")) ||
		bytes.Contains(data, []byte("-----BEGIN PRIVATE KEY-----")) ||
		bytes.Contains(data, []byte("-----BEGIN RSA PRIVATE KEY-----")) ||
		!utf8.Valid(data), nil
}

func validateConfigData(data []byte) error {
	dir, err := os.MkdirTemp("", "aks-flex-node-bootstrap-config-")
	if err != nil {
		return fmt.Errorf("create config validation directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("stage config validation: %w", err)
	}
	if _, err := config.LoadConfig(path); err != nil {
		return fmt.Errorf("validate rendered config: %w", err)
	}
	return nil
}
