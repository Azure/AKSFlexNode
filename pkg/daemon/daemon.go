package daemon

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/daemon"
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
	repaves, err := newRepaveReconciler(repaveReconcilerOptions{
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
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", repaves.nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}
	machineOperations, err := machineOperationReconciler(restCfg, mgr.GetClient(), log, repaves.nodeName, repaves.operator)
	if err != nil {
		return err
	}
	if err := daemon.SetupController("aks-flex-node-daemon", mgr, machineOperations, repaves); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}
	repaves.client = mgr.GetClient()

	err = mgr.Start(ctx)
	repaves.log.Info("daemon shutting down")
	return err
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
	agentCfg := config.ToAgentConfig(cfg, cfg.GetArcMachineName())
	if agentCfg.Kubelet.Auth.ExecCredential == nil {
		return nil, fmt.Errorf("daemon node client requires bootstrap token or exec credential")
	}
	restCfg.ExecProvider = &clientcmdapi.ExecConfig{
		APIVersion:         agentCfg.Kubelet.Auth.ExecCredential.APIVersion,
		Command:            agentCfg.Kubelet.Auth.ExecCredential.Command,
		Args:               agentCfg.Kubelet.Auth.ExecCredential.Args,
		Env:                agentCfg.Kubelet.Auth.ExecCredential.Env,
		InteractiveMode:    agentCfg.Kubelet.Auth.ExecCredential.InteractiveMode,
		ProvideClusterInfo: agentCfg.Kubelet.Auth.ExecCredential.ProvideClusterInfo,
	}
	return restCfg, nil
}
