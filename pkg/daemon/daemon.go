package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

// Run starts the ARM-machine-driven daemon loop. The AKS machine client is
// injected so production ARM, remote test, and local file-backed clients can
// share the same daemon controller.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, machines aksmachine.MachineClient) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		return err
	}
	kubeClient, err := client.New(restCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return fmt.Errorf("create controller-runtime client: %w", err)
	}
	store, err := newFileStateStore("")
	if err != nil {
		return err
	}
	controller, err := NewController(ControllerOptions{
		Log:                      log,
		Machines:                 machines,
		Client:                   kubeClient,
		Operator:                 NewNSpawnNodeOperator(cfg, store),
		NodeName:                 cfg.GetArcMachineName(),
		MachineReconcileInterval: cfg.Agent.MachineReconcileInterval,
	})
	if err != nil {
		return err
	}
	return controller.Run(ctx, restCfg)
}

func bootstrapCredentialRESTConfig(cfg *config.Config) (*rest.Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if cfg.Node.Kubelet.ServerURL == "" {
		return nil, fmt.Errorf("kubernetes API server URL is empty")
	}
	if cfg.Node.Kubelet.CACertData == "" {
		return nil, fmt.Errorf("kubernetes CA certificate data is empty")
	}
	if !cfg.IsBootstrapTokenConfigured() {
		return nil, fmt.Errorf("bootstrap token is required for daemon node client")
	}
	caData, err := base64.StdEncoding.DecodeString(cfg.Node.Kubelet.CACertData)
	if err != nil {
		return nil, fmt.Errorf("decode Kubernetes CA certificate: %w", err)
	}
	return &rest.Config{
		Host:        cfg.Node.Kubelet.ServerURL,
		BearerToken: cfg.Azure.BootstrapToken.Token,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}, nil
}
