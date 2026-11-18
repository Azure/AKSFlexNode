package daemon

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ValidateAgentDeployment checks the worker and agent deployment combination.
func ValidateAgentDeployment(mode isolation.Mode, containerized bool) error {
	if containerized && mode != isolation.ModeNSpawn {
		return fmt.Errorf("containerized agent does not yet support %s workers; use a host-installed agent", mode)
	}
	return nil
}

// ValidateWorkerBootstrap checks deployment compatibility and applied isolation.
func ValidateWorkerBootstrap(cfg *config.Config, applied *State, containerized bool) error {
	if err := ValidateAgentDeployment(cfg.Bootstrap.Isolation, containerized); err != nil {
		return err
	}
	if applied != nil && applied.Isolation != cfg.Bootstrap.Isolation {
		return fmt.Errorf("worker isolation change requires reset")
	}

	return nil
}

// BootstrapState selects the installed worker for a none/OCI reapply.
// Nspawn retains its existing bootstrap behavior.
func BootstrapState(applied, seeded *State, cfg *config.Config) (*State, bool, error) {
	switch cfg.Bootstrap.Isolation {
	case isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer:
	default:
		return nil, false, fmt.Errorf("unsupported worker isolation %q", cfg.Bootstrap.Isolation)
	}
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
