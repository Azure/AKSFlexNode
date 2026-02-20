package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"
	"go.goms.io/aks/AKSFlexNode/pkg/components/containerd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kube_binaries"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/components/npd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/runc"
	"go.goms.io/aks/AKSFlexNode/pkg/components/services"
	"go.goms.io/aks/AKSFlexNode/pkg/components/system_configuration"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

type MinimalBootstrapper struct {
	*BaseExecutor
}

func NewMinimal(cfg *config.Config, logger *logrus.Logger) *MinimalBootstrapper {
	return &MinimalBootstrapper{
		BaseExecutor: NewBaseExecutor(cfg, logger),
	}
}

func (b *MinimalBootstrapper) Bootstrap(ctx context.Context) (*ExecutionResult, error) {
	// Define the bootstrap steps in order - using modules directly
	steps := []Executor{
		system_configuration.NewInstaller(b.logger),
		runc.NewInstaller(b.logger),
		containerd.NewInstaller(b.logger),
		kube_binaries.NewInstaller(b.logger),
		kubelet.NewInstaller(b.logger),
		npd.NewInstaller(b.logger),
		services.NewInstaller(b.logger),
	}

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}

func (b *MinimalBootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	return nil, nil
}
