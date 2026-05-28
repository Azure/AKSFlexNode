package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	daemonCredentialDir   = "daemon-credentials"
	daemonCredentialGroup = "aks-flex-node-daemons"
)

// Run starts the ARM-machine-driven daemon loop. The AKS machine client is
// injected so production ARM, remote test, and local file-backed clients can
// share the same daemon controller.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, machines aksmachine.MachineClient) error {
	restCfg, stopCredentials, err := daemonRESTConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer stopCredentials()
	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	nodeName := cfg.Agent.NodeName
	// TODO: use the ARM machine resource name once the AKS RP Machine API contract is defined.
	aksMachineName := nodeName
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}
	operator, err := newNSpawnNodeOperator(cfg, store)
	if err != nil {
		return err
	}
	repaves, err := newRepaveReconciler(repaveReconcilerOptions{
		Log:                      log,
		Machines:                 machines,
		Client:                   mgr.GetClient(),
		Operator:                 operator,
		NodeName:                 nodeName,
		MachineReconcileInterval: time.Duration(cfg.Agent.MachineReconcileInterval),
	})
	if err != nil {
		return err
	}
	machineOperations, err := machineOperationReconciler(machineOperationReconcilerOptions{
		Client:               mgr.GetClient(),
		Log:                  log,
		NodeName:             nodeName,
		AKSMachineName:       aksMachineName,
		MachineOperationMode: cfg.Agent.MachineOperationMode,
		Operator:             repaves.operator,
	})
	if err != nil {
		return err
	}
	if err := daemon.SetupController("aks-flex-node-daemon", mgr, machineOperations, repaves); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}

	err = mgr.Start(ctx)
	repaves.log.Info("daemon shutting down")
	return err
}

func daemonRESTConfig(ctx context.Context, cfg *config.Config) (*rest.Config, func(), error) {
	bootstrapRestCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	if !cfg.IsBootstrapTokenConfigured() {
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
	}
}

func bootstrapCredentialRESTConfig(cfg *config.Config) (*rest.Config, error) {
	if cfg.Node.Kubelet.ServerURL == "" {
		return nil, fmt.Errorf("kubernetes API server URL is empty")
	}
	if cfg.Node.Kubelet.CACertData == "" {
		return nil, fmt.Errorf("kubernetes CA certificate data is empty")
	}
	caData, err := base64.StdEncoding.DecodeString(cfg.Node.Kubelet.CACertData)
	if err != nil {
		return nil, fmt.Errorf("decode Kubernetes CA certificate: %w", err)
	}
	restCfg := &rest.Config{
		Host: cfg.Node.Kubelet.ServerURL,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caData,
		},
	}
	if cfg.IsBootstrapTokenConfigured() {
		restCfg.BearerToken = cfg.Azure.BootstrapToken.Token
		return restCfg, nil
	}
	agentCfg := config.ToAgentConfig(cfg, cfg.Agent.NodeName)
	if agentCfg.Kubelet.Auth.ExecCredential == nil {
		return nil, fmt.Errorf("daemon node client requires bootstrap token or exec credential")
	}
	restCfg.ExecProvider = agentCfg.Kubelet.Auth.ExecCredential.DeepCopy()
	return restCfg, nil
}
