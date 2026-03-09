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
		installArc.Executor("install-arc", b.componentsAPIConn),
		configureSystem.Executor("configure-os", b.componentsAPIConn),
		// Some environments might have docker pre-installed which can interfere with Kubernetes networking.
		// This step disables the docker services and configures the docker daemon to not manage iptables rules.
		disableDocker.Executor("disable-docker", b.componentsAPIConn),

		// Fetch serverURL and caCertData from AKS cluster admin credentials for
		// non-bootstrap-token auth modes (Arc, SP, MI). Must run before startKubelet
		// and startNPD which require these fields.
		newClusterConfigEnricher(b.logger),

		// TODO: run these steps in parallel
		downloadCNIBinaries.Executor("download-cni-binaries", b.componentsAPIConn),
		downloadCRIBinaries.Executor("download-cri-binaries", b.componentsAPIConn),
		downloadKubeBinaries.Executor("download-kube-binaries", b.componentsAPIConn),
		downloadNPD.Executor("download-npd", b.componentsAPIConn),

		configureCNI.Executor("configure-cni", b.componentsAPIConn),
		startContainerdService.Executor("start-containerd", b.componentsAPIConn),
		// Configure iptables rules before kubelet starts to prevent conflicts with Kubernetes networking
		configureIPTables.Executor("configure-iptables", b.componentsAPIConn),
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
