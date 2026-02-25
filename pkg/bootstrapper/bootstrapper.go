package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"go.goms.io/aks/AKSFlexNode/pkg/components/arc"
	"go.goms.io/aks/AKSFlexNode/pkg/components/cni"
	"go.goms.io/aks/AKSFlexNode/pkg/components/containerd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kube_binaries"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/components/npd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/runc"
	"go.goms.io/aks/AKSFlexNode/pkg/components/services"
	"go.goms.io/aks/AKSFlexNode/pkg/components/system_configuration"
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
		// system_configuration.NewInstaller(b.logger), // Configure system (early)

		downloadCRIBinaries.Executor("download-cri-binaries", b.componentsAPIConn),
		downloadKubeBinaries.Executor("download-kube-binaries", b.componentsAPIConn),
		downloadNPD.Executor("download-npd", b.componentsAPIConn),

		startContainerdService.Executor("start-containerd", b.componentsAPIConn),
		startKubelet.Executor("start-kubelet", b.componentsAPIConn),
		startNPD.Executor("start-npd", b.componentsAPIConn),

		// kubelet.NewInstaller(b.logger), // Configure kubelet service with Arc MSI auth
	}

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}

// Unbootstrap executes all cleanup steps sequentially (in reverse order of bootstrap)
func (b *Bootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	steps := []Executor{
		services.NewUnInstaller(b.logger),             // Stop services first
		npd.NewUnInstaller(b.logger),                  // Uninstall Node Problem Detector
		kubelet.NewUnInstaller(b.logger),              // Clean kubelet configuration
		cni.NewUnInstaller(b.logger),                  // Clean CNI configs
		kube_binaries.NewUnInstaller(b.logger),        // Uninstall k8s binaries
		containerd.NewUnInstaller(b.logger),           // Uninstall containerd binary
		runc.NewUnInstaller(b.logger),                 // Uninstall runc binary
		system_configuration.NewUnInstaller(b.logger), // Clean system settings
		arc.NewUnInstaller(b.logger),                  // Uninstall Arc (after cleanup)
	}

	return b.ExecuteSteps(ctx, steps, "unbootstrap")
}
