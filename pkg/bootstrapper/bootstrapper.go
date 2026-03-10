package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/Azure/AKSFlexNode/pkg/components/arc"
	"github.com/Azure/AKSFlexNode/pkg/config"
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
		installArc.Executor("install-arc", b.componentsAPIConn, b.config),
		configureSystem.Executor("configure-os", b.componentsAPIConn, b.config),
		// Some environments might have docker pre-installed which can interfere with Kubernetes networking.
		// This step disables the docker services and configures the docker daemon to not manage iptables rules.
		disableDocker.Executor("disable-docker", b.componentsAPIConn, b.config),

		// Fetch serverURL and caCertData from AKS cluster admin credentials for
		// non-bootstrap-token auth modes (Arc, SP, MI). Must run before startKubelet
		// and startNPD which require these fields.
		newClusterConfigEnricher(b.logger),

		// TODO: run these steps in parallel
		downloadCNIBinaries.Executor("download-cni-binaries", b.componentsAPIConn, b.config),
		downloadCRIBinaries.Executor("download-cri-binaries", b.componentsAPIConn, b.config),
		downloadKubeBinaries.Executor("download-kube-binaries", b.componentsAPIConn, b.config),
		downloadNPD.Executor("download-npd", b.componentsAPIConn, b.config),

		configureCNI.Executor("configure-cni", b.componentsAPIConn, b.config),
		startContainerdService.Executor("start-containerd", b.componentsAPIConn, b.config),
		// Configure iptables rules before kubelet starts to prevent conflicts with Kubernetes networking
		configureIPTables.Executor("configure-iptables", b.componentsAPIConn, b.config),
		startKubelet.Executor("start-kubelet", b.componentsAPIConn, b.config),
		startNPD.Executor("start-npd", b.componentsAPIConn, b.config),
	}

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}

// Unbootstrap executes all cleanup steps sequentially (in reverse order of bootstrap)
func (b *Bootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	steps := []Executor{
		resetKubelet.Executor("reset-kubelet", b.componentsAPIConn),
		resetContainerdService.Executor("reset-containerd", b.componentsAPIConn),
		arc.NewUnInstaller(b.logger), // Uninstall Arc (after cleanup)
	}

	return b.ExecuteSteps(ctx, steps, "unbootstrap")
}
