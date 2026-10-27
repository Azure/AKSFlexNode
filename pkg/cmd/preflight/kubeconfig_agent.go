package preflight

import (
	"context"
	"fmt"

	"github.com/Azure/AKSFlexNode/pkg/config"
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
	return append(checks, agentKubeconfigChecker{cfg: cfg}), nil
}

type agentKubeconfigChecker struct {
	cfg *config.Config
}

func (c agentKubeconfigChecker) Name() string {
	return checkAgentKubeconfigName
}

func (c agentKubeconfigChecker) Check(context.Context) []preflight.Result {
	const target = "host agent Kubernetes client"

	if _, err := kubeauth.BootstrapRESTConfig(c.cfg); err != nil {
		return preflight.ResultsError(c.Name(), target, "%v", err)
	}
	return preflight.ResultsOK(c.Name(), target, "host agent credential configuration is valid")
}
