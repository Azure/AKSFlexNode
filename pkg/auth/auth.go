package auth

import (
	"context"
	"fmt"
	"os"
	"os/exec"

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
		cfg.Azure.ServicePrincipal.TenantID,
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

// CheckCLIAuthStatus checks if user is logged in to Azure CLI and if the token is valid
func (a *AuthProvider) CheckCLIAuthStatus(ctx context.Context) error {
	// Try to get account information - this will fail if not logged in or token expired
	cmd := exec.CommandContext(ctx, "az", "account", "show", "--output", "json")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Azure CLI authentication check failed: %w", err)
	}

	// Try to get an access token to verify it's not expired
	cmd = exec.CommandContext(ctx, "az", "account", "get-access-token", "--output", "json")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Azure CLI token validation failed: %w", err)
	}

	return nil
}

// InteractiveAzLogin performs interactive Azure CLI login with tenant ID and proper console tunneling
func (a *AuthProvider) InteractiveAzLogin(ctx context.Context, tenantID string) error {
	// Build az login command with tenant ID
	args := []string{"login", "--tenant", tenantID}

	// Create command with proper console I/O tunneling
	cmd := exec.CommandContext(ctx, "az", args...)

	// Connect stdin, stdout, stderr to allow interactive prompts
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the interactive login command
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("interactive Azure CLI login failed: %w", err)
	}

	return nil
}

// EnsureAuthenticated checks if user is authenticated and prompts for login if needed
func (a *AuthProvider) EnsureAuthenticated(ctx context.Context, tenantID string) error {
	// Check if already authenticated with valid token
	if err := a.CheckCLIAuthStatus(ctx); err == nil {
		return nil // Already authenticated and token is valid
	}

	// Not authenticated or token expired, prompt for interactive login
	return a.InteractiveAzLogin(ctx, tenantID)
}
