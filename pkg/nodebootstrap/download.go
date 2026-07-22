package nodebootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	DefaultStorageScope = "https://storage.azure.com/.default"
	defaultStorageAPI   = "2023-11-03"
)

// StorageAuthOptions configures authentication used only to retrieve bootstrap
// inputs. These credentials are never copied into the agent configuration.
type StorageAuthOptions struct {
	Mode             string
	TenantID         string
	ClientID         string
	ClientSecret     string
	ClientSecretFile string
	AuthorityHost    string
	TokenScope       string
}

// Downloader retrieves local, file, and HTTP bootstrap inputs.
type Downloader struct {
	client     *http.Client
	credential azcore.TokenCredential
	tokenScope string
}

// NewDownloader constructs a downloader with the selected Azure Storage
// authentication method. SAS authentication is carried by the source URL.
func NewDownloader(options StorageAuthOptions) (*Downloader, error) {
	credential, err := newStorageCredential(options)
	if err != nil {
		return nil, err
	}
	scope := strings.TrimSpace(options.TokenScope)
	if scope == "" {
		scope = DefaultStorageScope
	}
	return &Downloader{
		client: &http.Client{
			Timeout: 10 * time.Minute,
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				if len(via) == 0 {
					return nil
				}
				if request.URL.Scheme != "https" &&
					(via[0].Header.Get("Authorization") != "" || via[0].URL.Query().Get("sig") != "") {
					return fmt.Errorf("refusing authenticated download redirect to non-HTTPS URL")
				}
				if !strings.EqualFold(request.URL.Host, via[0].URL.Host) {
					request.Header.Del("Authorization")
					request.Header.Del("x-ms-version")
				}
				return nil
			},
		},
		credential: credential,
		tokenScope: scope,
	}, nil
}

func newStorageCredential(options StorageAuthOptions) (azcore.TokenCredential, error) {
	mode := strings.ToLower(strings.TrimSpace(options.Mode))
	switch mode {
	case "", "none", "sas":
		return nil, nil
	case "sp", "service-principal":
		secret, err := loadClientSecret(options)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(options.TenantID) == "" {
			return nil, fmt.Errorf("storage tenant ID is required for service-principal authentication")
		}
		if strings.TrimSpace(options.ClientID) == "" {
			return nil, fmt.Errorf("storage client ID is required for service-principal authentication")
		}
		credentialOptions := &azidentity.ClientSecretCredentialOptions{}
		if authorityHost := strings.TrimSpace(options.AuthorityHost); authorityHost != "" {
			credentialOptions.Cloud = cloud.Configuration{ActiveDirectoryAuthorityHost: authorityHost}
		}
		credential, err := azidentity.NewClientSecretCredential(
			strings.TrimSpace(options.TenantID),
			strings.TrimSpace(options.ClientID),
			secret,
			credentialOptions,
		)
		if err != nil {
			return nil, fmt.Errorf("create storage service-principal credential: %w", err)
		}
		return credential, nil
	case "msi", "managed-identity":
		credentialOptions := &azidentity.ManagedIdentityCredentialOptions{}
		if clientID := strings.TrimSpace(options.ClientID); clientID != "" {
			credentialOptions.ID = azidentity.ClientID(clientID)
		}
		credential, err := azidentity.NewManagedIdentityCredential(credentialOptions)
		if err != nil {
			return nil, fmt.Errorf("create storage managed-identity credential: %w", err)
		}
		return credential, nil
	default:
		return nil, fmt.Errorf("unsupported storage authentication mode %q: expected none, sas, service-principal, or msi", options.Mode)
	}
}

func loadClientSecret(options StorageAuthOptions) (string, error) {
	if secretFile := strings.TrimSpace(options.ClientSecretFile); secretFile != "" {
		info, err := os.Stat(secretFile)
		if err != nil {
			return "", fmt.Errorf("stat storage client secret file: %w", err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("storage client secret file %s must not be accessible by group or other users", secretFile)
		}
		data, err := os.ReadFile(filepath.Clean(secretFile))
		if err != nil {
			return "", fmt.Errorf("read storage client secret file: %w", err)
		}
		secret := strings.TrimRight(string(data), "\r\n")
		if secret == "" {
			return "", fmt.Errorf("storage client secret file is empty")
		}
		return secret, nil
	}
	if options.ClientSecret != "" {
		return options.ClientSecret, nil
	}
	return "", fmt.Errorf("storage client secret file is required for service-principal authentication")
}

// Fetch reads source with an upper bound. Local paths and file:// URLs never
// receive Azure authentication headers.
func (d *Downloader) Fetch(ctx context.Context, source string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("download limit must be positive")
	}
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse download source: invalid URL")
	}
	if parsed.Scheme == "" {
		return readLocalFile(source, maxBytes)
	}
	if parsed.Scheme == "file" {
		if parsed.Host != "" && parsed.Host != "localhost" {
			return nil, fmt.Errorf("file download source must not contain a remote host")
		}
		path, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return nil, fmt.Errorf("decode file download source: %w", err)
		}
		return readLocalFile(path, maxBytes)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported download source scheme %q", parsed.Scheme)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("create download request: invalid URL")
	}
	// A signed URL is already authenticated. Omitting the bearer token also
	// allows a SAS config and an MSI/SP artifact to share one downloader.
	hasSAS := parsed.Query().Get("sig") != ""
	useBearerToken := d.credential != nil && !hasSAS
	if (hasSAS || useBearerToken) && parsed.Scheme != "https" {
		return nil, fmt.Errorf("authenticated Azure Storage downloads require HTTPS")
	}
	if useBearerToken {
		token, err := d.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{d.tokenScope}})
		if err != nil {
			return nil, fmt.Errorf("acquire Azure Storage token: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+token.Token)
		request.Header.Set("x-ms-version", defaultStorageAPI)
	}
	response, err := d.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download source request: %w", redactDownloadError(err))
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = response.Body.Close()
		return nil, fmt.Errorf("download source returned HTTP status %d", response.StatusCode)
	}
	data, readErr := readLimited(response.Body, maxBytes)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close download source: %w", closeErr)
	}
	return data, nil
}

func redactDownloadError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func readLocalFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("open local download source: %w", err)
	}
	data, readErr := readLimited(file, maxBytes)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close local download source: %w", closeErr)
	}
	return data, nil
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read download source: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("download source exceeds %d bytes", maxBytes)
	}
	return data, nil
}
