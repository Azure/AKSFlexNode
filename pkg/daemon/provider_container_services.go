package daemon

import (
	"context"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/provider"
	"github.com/Azure/unbounded/pkg/agent/containerops"
)

func newContainerProvider() NodeProvider {
	startup := defaultContainerStartupStore()
	return provider.Container{
		Services: containerProviderServices(startup, agentUpgradeLockPath),
		Startup:  startup, StartupPath: startup.path, ActivationLockPath: agentUpgradeLockPath,
		BinaryPath: provider.ContainerAgentBinaryPath, Teardown: &containerops.Teardown{},
	}
}

func containerProviderServices(startup containerStartupStore, activationLockPath string) provider.ContainerServices {
	return provider.ContainerServices{
		HostServices: hostProviderServices(),
		NewOperator: func(ctx context.Context, cfg *config.Config, store stateStore, worker NodeProvider) (nodeOperator, error) {
			base, state, err := newOperatorBase(ctx, cfg, store, worker)
			if err != nil {
				return nil, err
			}
			return newContainerNodeOperator(base, state, startup, activationLockPath)
		},
		NewNode: newContainerNode, CheckBootstrap: checkContainerBootstrap,
		SetupHost: setupContainerHost, StartNode: startContainerNode, ResetNode: resetContainerNode,
		RewriteNodeUnits: rewriteContainerHostRootUnits,
	}
}
