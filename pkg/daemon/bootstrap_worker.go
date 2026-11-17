package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ValidateAgentDeployment checks the worker and agent deployment combination.
func ValidateAgentDeployment(mode isolation.Mode, containerized bool) error {
	if containerized && mode != isolation.ModeNSpawn {
		return fmt.Errorf("containerized agent does not yet support %s workers; use a host-installed agent", mode)
	}
	return nil
}

// ValidateWorkerBootstrap refuses an isolation switch or incompatible host before mutation.
func ValidateWorkerBootstrap(ctx context.Context, cfg *config.Config, log *slog.Logger, applied *State, containerized bool) error {
	if err := ValidateAgentDeployment(cfg.Bootstrap.Isolation, containerized); err != nil {
		return err
	}
	if applied != nil && applied.Isolation != cfg.Bootstrap.Isolation {
		return fmt.Errorf("worker isolation change requires reset")
	}
	artifacts, err := agentruntime.NoneUpgradeArtifactsPresent()
	if err != nil {
		return err
	}
	present := artifacts || noneops.Present()
	ociArtifacts, err := agentruntime.ContainerUpgradeArtifactPresent()
	if err != nil {
		return err
	}
	ociPresent := ociArtifacts || containerops.Present()
	if cfg.Bootstrap.Isolation != isolation.ModeContainer && ociPresent {
		return fmt.Errorf("OCI worker is present; reset before changing isolation")
	}
	if cfg.Bootstrap.Isolation == isolation.ModeContainer {
		if present {
			return fmt.Errorf("host-native worker is present; reset before changing isolation")
		}
		if applied == nil && ociPresent {
			return fmt.Errorf("OCI worker has no applied state; reset before bootstrap")
		}
		return host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNSpawn)
	}
	if cfg.Bootstrap.Isolation == isolation.ModeNone {
		if applied == nil && present {
			return fmt.Errorf("host-native worker has no applied state; reset before bootstrap")
		}
		if err := host.EnsureNoExistingDeploymentFor(ctx, log, isolation.ModeNSpawn); err != nil {
			return err
		}
		return none.NewDefault(log).PreflightOwnership()
	}
	if present {
		return fmt.Errorf("host-native worker is present; reset before changing isolation")
	}
	return nil
}

// BootstrapState selects the installed worker for a none/OCI reapply.
// Nspawn retains its existing bootstrap behavior.
func BootstrapState(applied, seeded *State, cfg *config.Config) (*State, bool, error) {
	if applied == nil || cfg.Bootstrap.Isolation == isolation.ModeNSpawn {
		return seeded, false, nil
	}
	if !validActiveMachine(applied.ActiveMachine) || applied.NodeConfig == nil {
		return nil, false, fmt.Errorf("applied worker configuration is incomplete")
	}
	if err := validateReapplyIdentity(cfg, applied); err != nil {
		return nil, false, err
	}
	return applied, true, nil
}

func validateReapplyIdentity(cfg *config.Config, applied *State) error {
	installed := applied.NodeConfig
	candidate := config.ToAgentConfig(cfg, applied.ActiveMachine)
	if installed.NodeName != candidate.NodeName {
		return fmt.Errorf("reapply node name differs from the installed worker; reset is required")
	}
	if strings.TrimRight(installed.Kubelet.ApiServer, "/") != strings.TrimRight(candidate.Kubelet.ApiServer, "/") ||
		installed.Cluster.CaCertBase64 != candidate.Cluster.CaCertBase64 {
		return fmt.Errorf("reapply cluster endpoint or CA differs from the installed worker; reset is required")
	}
	if !sameWorkerAuth(installed.Kubelet, candidate.Kubelet) {
		return fmt.Errorf("reapply worker authentication differs from the installed worker; reset is required")
	}
	return nil
}

func sameWorkerAuth(installed, candidate agentconfig.AgentKubeletConfig) bool {
	switch {
	case len(installed.KubeconfigData) > 0:
		return bytes.Equal(installed.KubeconfigData, candidate.KubeconfigData)
	case installed.Auth.BootstrapToken != "":
		// Bootstrap tokens are short-lived and may have been refreshed on repave.
		return candidate.Auth.BootstrapToken != "" && len(candidate.KubeconfigData) == 0
	case installed.Auth.ExecCredential != nil && candidate.Auth.ExecCredential != nil:
		old, next := installed.Auth.ExecCredential, candidate.Auth.ExecCredential
		return old.Command == next.Command && slices.Equal(old.Args, next.Args) && maps.Equal(execIdentity(old.Env), execIdentity(next.Env))
	default:
		return false
	}
}

func execIdentity(env []clientcmdapi.ExecEnvVar) map[string]string {
	identity := make(map[string]string)
	for _, entry := range env {
		switch entry.Name {
		case "AAD_LOGIN_METHOD", "AAD_SERVICE_PRINCIPAL_CLIENT_ID", "AZURE_TENANT_ID", "AZURE_CLIENT_ID":
			identity[entry.Name] = entry.Value
		}
	}
	return identity
}
