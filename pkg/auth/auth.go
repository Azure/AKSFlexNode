package auth

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// AuthProvider is a simple factory for Azure credentials
type AuthProvider struct{}

// NewAuthProvider creates a new authentication provider
func NewAuthProvider() *AuthProvider {
	return &AuthProvider{}
}

// ArcCredential returns Azure Arc managed identity credential
func (a *AuthProvider) ArcCredential() (azcore.TokenCredential, error) {
	cred, err := azidentity.NewManagedIdentityCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create Arc credential: %w", err)
	}
	return cred, nil
}

// UserCredential returns credential based on config (service principal or CLI fallback)
func (a *AuthProvider) UserCredential(ctx context.Context, cfg *config.Config) (azcore.TokenCredential, error) {
	if cfg.IsSPConfigured() {
		return a.serviceCredential(cfg)
	}
	return a.cliCredential()
}

// serviceCredential creates service principal credential from config
func (a *AuthProvider) serviceCredential(cfg *config.Config) (azcore.TokenCredential, error) {
	cred, err := azidentity.NewClientSecretCredential(
		cfg.Azure.TenantID,
		cfg.Azure.ServicePrincipal.ClientID,
		cfg.Azure.ServicePrincipal.ClientSecret,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create service principal credential: %w", err)
	}
	return cred, nil
}

// cliCredential creates Azure CLI credential
func (a *AuthProvider) cliCredential() (azcore.TokenCredential, error) {
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create CLI credential: %w", err)
	}
	return cred, nil
}

// GetAccessToken retrieves access token for given credential with default ARM scope
func (a *AuthProvider) GetAccessToken(ctx context.Context, cred azcore.TokenCredential) (string, error) {
	return a.GetAccessTokenForResource(ctx, cred, "https://management.azure.com/.default")
}

// GetAccessTokenForResource retrieves access token for given credential and resource
func (a *AuthProvider) GetAccessTokenForResource(ctx context.Context, cred azcore.TokenCredential, resource string) (string, error) {
	tokenRequestOptions := policy.TokenRequestOptions{
		Scopes: []string{resource},
	}

	accessToken, err := cred.GetToken(ctx, tokenRequestOptions)
	if err != nil {
		return "", fmt.Errorf("failed to get access token: %w", err)
	}

	return accessToken.Token, nil
}
