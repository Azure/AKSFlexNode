package nodebootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type staticTokenCredential struct{}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (staticTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "storage-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

func TestDownloaderFetchHTTPAuthentication(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		query          string
		wantAuthorized bool
	}{
		"bearer token": {wantAuthorized: true},
		"SAS URL":      {query: "?sig=signed", wantAuthorized: false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				gotAuthorized := request.Header.Get("Authorization") == "Bearer storage-token"
				if gotAuthorized != test.wantAuthorized {
					t.Errorf("Authorization present = %t, want %t", gotAuthorized, test.wantAuthorized)
				}
				if test.wantAuthorized && request.Header.Get("x-ms-version") == "" {
					t.Error("x-ms-version header is empty")
				}
				_, _ = writer.Write([]byte("payload"))
			}))
			defer server.Close()

			downloader := &Downloader{
				client:     server.Client(),
				credential: staticTokenCredential{},
				tokenScope: DefaultStorageScope,
			}
			data, err := downloader.Fetch(context.Background(), server.URL+test.query, 1024)
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if string(data) != "payload" {
				t.Fatalf("Fetch() = %q, want payload", data)
			}
		})
	}
}

func TestDownloaderRedactsSignedURLFromErrors(t *testing.T) {
	t.Parallel()

	downloader := &Downloader{
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})},
		tokenScope: DefaultStorageScope,
	}
	_, err := downloader.Fetch(context.Background(), "https://account.example/config?sig=top-secret", 1024)
	if err == nil {
		t.Fatal("Fetch() error = nil, want network error")
	}
	if strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("Fetch() error contains signed URL secret: %v", err)
	}
}

func TestDownloaderFetchLocalFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	downloader, err := NewDownloader(StorageAuthOptions{Mode: "none"})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}

	for name, source := range map[string]string{
		"path":     path,
		"file URL": "file://" + path,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data, err := downloader.Fetch(context.Background(), source, 1024)
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if string(data) != "payload" {
				t.Fatalf("Fetch() = %q, want payload", data)
			}
		})
	}
}

func TestDownloaderRejectsOversizedInput(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(path, []byte("too large"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	downloader, err := NewDownloader(StorageAuthOptions{})
	if err != nil {
		t.Fatalf("NewDownloader() error = %v", err)
	}
	_, err = downloader.Fetch(context.Background(), path, 3)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Fetch() error = %v, want size error", err)
	}
}

func TestManagedIdentityDownloader(t *testing.T) {
	t.Parallel()

	tests := map[string]StorageAuthOptions{
		"system assigned": {Mode: "msi"},
		"user assigned":   {Mode: "managed-identity", ClientID: "00000000-0000-0000-0000-000000000001"},
	}
	for name, options := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewDownloader(options); err != nil {
				t.Fatalf("NewDownloader() error = %v", err)
			}
		})
	}
}

func TestServicePrincipalSecretFilePermissions(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mode    os.FileMode
		wantErr bool
	}{
		"protected":      {mode: 0o600},
		"group readable": {mode: 0o640, wantErr: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(path, []byte("secret"), test.mode); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}
			_, err := newStorageCredential(StorageAuthOptions{
				Mode:             "service-principal",
				TenantID:         "tenant",
				ClientID:         "client",
				ClientSecretFile: path,
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("newStorageCredential() error = %v, wantErr %t", err, test.wantErr)
			}
		})
	}
}
