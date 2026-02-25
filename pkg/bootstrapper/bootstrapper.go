package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"go.goms.io/aks/AKSFlexNode/pkg/components/arc"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// Bootstrapper executes bootstrap steps sequentially
type Bootstrapper struct {
	*BaseExecutor

	componentsAPIConn *grpc.ClientConn
}

// New creates a new bootstrapper
func New(
	cfg *config.Config,
	logger *logrus.Logger,
	componentsAPIConn *grpc.ClientConn,
) *Bootstrapper {
	return &Bootstrapper{
		BaseExecutor:      NewBaseExecutor(cfg, logger),
		componentsAPIConn: componentsAPIConn,
	}
}

// Bootstrap executes all bootstrap steps sequentially
func (b *Bootstrapper) Bootstrap(ctx context.Context) (*ExecutionResult, error) {
	// Define the bootstrap steps in order - using modules directly
	steps := []Executor{
		arc.NewInstaller(b.logger), // Setup Arc

		configureSystem.Executor("configure-os", b.componentsAPIConn),
		// FIXME: confirm if it's really needed to update the resolved config
		// system_configuration.NewInstaller(b.logger), // Configure system (early)

		// TODO: run these steps in parallel
		downloadCNIBinaries.Executor("download-cni-binaries", b.componentsAPIConn),
		downloadCRIBinaries.Executor("download-cri-binaries", b.componentsAPIConn),
		downloadKubeBinaries.Executor("download-kube-binaries", b.componentsAPIConn),
		downloadNPD.Executor("download-npd", b.componentsAPIConn),

		startContainerdService.Executor("start-containerd", b.componentsAPIConn),
		startKubelet.Executor("start-kubelet", b.componentsAPIConn),
		startNPD.Executor("start-npd", b.componentsAPIConn),
	}

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}

// Unbootstrap executes all cleanup steps sequentially (in reverse order of bootstrap)
func (b *Bootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	steps := []Executor{
		arc.NewUnInstaller(b.logger), // Uninstall Arc (after cleanup)
	}

	return b.ExecuteSteps(ctx, steps, "unbootstrap")
}
