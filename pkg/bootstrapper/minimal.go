package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/components/kube_binaries"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubeadm"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

type MinimalBootstrapper struct {
	config *config.Config

	*BaseExecutor
}

// NewMinimal creates a new minimal bootstrapper with only essential steps for joining the cluster.
func NewMinimal(cfg *config.Config, logger *logrus.Logger) *MinimalBootstrapper {
	return &MinimalBootstrapper{
		config:       cfg,
		BaseExecutor: NewBaseExecutor(cfg, logger),
	}
}

func (b *MinimalBootstrapper) Bootstrap(ctx context.Context) (*ExecutionResult, error) {
	kubeadmNodeJoin, err := kubeadm.NewNodeJoin(b.config)
	if err != nil {
		return nil, err
	}

	steps := []Executor{
		kube_binaries.NewInstaller(b.logger), // for installing kubeadm
		kubeadmNodeJoin,
	}

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}
