package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"

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
}

// New creates a new bootstrapper
func New(cfg *config.Config, logger *logrus.Logger) *Bootstrapper {
	return &Bootstrapper{
		BaseExecutor: NewBaseExecutor(cfg, logger),
	}
}

// Bootstrap executes all bootstrap steps sequentially
func (b *Bootstrapper) Bootstrap(ctx context.Context) (*ExecutionResult, error) {
	// Define the bootstrap steps in order - using modules directly
	steps := []Executor{
		arc.NewInstaller(b.logger),                               // Setup Arc
		services.NewUnInstaller(b.logger),                        // Stop kubelet before setup
		system_configuration.NewInstaller(b.logger),              // Configure system (early)
		runc.NewInstaller(b.logger),                              // Install runc
		containerd.NewInstaller(b.logger),                        // Install containerd
		kube_binaries.NewInstallerWithConfig(b.config, b.logger), // Install k8s binaries
		cni.NewInstaller(b.logger),                               // Setup CNI (after container runtime)
		kubelet.NewInstallerWithConfig(b.config, b.logger),       // Configure kubelet service with Arc MSI auth
		npd.NewInstaller(b.logger),                               // Install Node Problem Detector
		services.NewInstaller(b.logger),                          // Start services
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
