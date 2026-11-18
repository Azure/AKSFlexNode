package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/kubeauth"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	daemonCredentialDir   = "daemon-credentials"    //nolint:gosec // Directory name, not a credential.
	daemonCredentialGroup = "aks-flex-node-daemons" //nolint:gosec // Kubernetes group name, not a credential.
)

func daemonRESTConfig(ctx context.Context, cfg *config.Config) (*rest.Config, func(), error) {
	plugin, err := agentdeployment.CredentialPlugin()
	if err != nil {
		return nil, nil, err
	}
	bootstrapRestCfg, err := bootstrapCredentialRESTConfig(cfg, plugin)
	if err != nil {
		return nil, nil, err
	}
	if cfg.UsesKubeconfigCredentials() || !cfg.IsBootstrapTokenConfigured() {
		return bootstrapRestCfg, func() {}, nil
	}
	credentials, stop, err := daemonRESTConfigProvider(ctx, cfg, bootstrapRestCfg)
	if err != nil {
		return nil, nil, err
	}
	return credentials.RESTConfig(), stop, nil
}

func daemonRESTConfigProvider(ctx context.Context, cfg *config.Config, base *rest.Config) (*daemoncred.RESTConfigProvider, func(), error) {
	credentialDir := filepath.Join(config.ConfigDir, daemonCredentialDir)
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create daemon credential directory: %w", err)
	}
	credentialCtx, stop := context.WithCancel(ctx)
	provider, err := daemoncred.NewRESTConfigProvider(credentialCtx, base, cfg.Agent.NodeName, daemonControllerCertificateOptions(credentialDir))
	if err != nil {
		stop()
		return nil, nil, fmt.Errorf("create daemon credential provider: %w", err)
	}
	go provider.Run(credentialCtx)
	return provider, stop, nil
}

func daemonControllerCertificateOptions(credentialDir string) daemoncred.ControllerCertificateOptions {
	return daemoncred.ControllerCertificateOptions{
		Name:          "aks-flex-node-daemon",
		DaemonGroup:   daemonCredentialGroup,
		CredentialDir: credentialDir,
		WaitTimeout:   2 * time.Minute,
	}
}

func retireDaemonCredentials() error {
	return retireDaemonCredentialsAt(filepath.Join(config.ConfigDir, daemonCredentialDir))
}

func retireDaemonCredentialsAt(credentialDir string) error {
	if err := os.RemoveAll(credentialDir); err != nil {
		return fmt.Errorf("remove retired daemon credential directory: %w", err)
	}
	return nil
}

// bootstrapCredentialRESTConfig builds the client the daemon uses until its own
// certificate is issued. An exec credential runs plugin; see agentdeployment.CredentialPlugin.
func bootstrapCredentialRESTConfig(cfg *config.Config, plugin string) (*rest.Config, error) {
	return kubeauth.BootstrapRESTConfigWithPlugin(cfg, plugin)
}
