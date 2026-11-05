package preflight

import (
	"context"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/kubeauth"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkAgentKubeconfigName = "agent-kubeconfig"

func pairedKubeconfigChecks(cfg *config.Config, checks []preflight.Checker) ([]preflight.Checker, error) {
	replaced := false
	for i, check := range checks {
		if check.Name() == checkAPIServerReachableName {
			checks[i] = kubeconfigAPIServerReachableChecker{data: cfg.Node.Kubelet.KubeconfigData}
			replaced = true
		}
	}
	if !replaced {
		return nil, fmt.Errorf("upstream node preflight is missing check %q", checkAPIServerReachableName)
	}
	// The plugin start's clients run, so preflight checks the credential they'll use.
	plugin, err := daemon.CredentialPlugin()
	if err != nil {
		return nil, fmt.Errorf("find the agent's credential plugin: %w", err)
	}
	return append(checks, agentKubeconfigChecker{cfg: cfg, plugin: plugin}), nil
}

type agentKubeconfigChecker struct {
	cfg    *config.Config
	plugin string
}

func (c agentKubeconfigChecker) Name() string {
	return checkAgentKubeconfigName
}

func (c agentKubeconfigChecker) Check(context.Context) []preflight.Result {
	const target = "host agent Kubernetes client"

	if _, err := kubeauth.BootstrapRESTConfigWithPlugin(c.cfg, c.plugin); err != nil {
		return preflight.ResultsError(c.Name(), target, "%v", err)
	}
	return preflight.ResultsOK(c.Name(), target, "host agent credential configuration is valid")
}
