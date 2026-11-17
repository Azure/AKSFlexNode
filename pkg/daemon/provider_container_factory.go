package daemon

import (
	"context"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/containerops"
)

func newContainerProvider() runtimeprovider.NodeProvider {
	startup := defaultContainerStartupStore()
	return runtimeprovider.Container{
		Host: hostProviderServices(), OperatorFactory: newContainerOperatorFactory(startup, agentUpgradeLockPath),
		PlannedHostRoot: PlannedHostRoot,
		Startup:         startup, StartupPath: startup.path, ActivationLockPath: agentUpgradeLockPath,
		BinaryPath: runtimeprovider.ContainerAgentBinaryPath, Teardown: &containerops.Teardown{},
	}
}

func newContainerOperatorFactory(startup containerStartupStore, activationLockPath string) runtimeprovider.OperatorFactory {
	return func(ctx context.Context, cfg *config.Config, store stateStore, worker runtimeprovider.NodeProvider) (runtimeprovider.Operator, error) {
		base, state, err := newOperatorBase(ctx, cfg, store, worker)
		if err != nil {
			return nil, err
		}
		return newContainerNodeOperator(base, state, startup, activationLockPath)
	}
}
