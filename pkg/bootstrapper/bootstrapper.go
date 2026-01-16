package bootstrapper

import (
	"context"

	"github.com/sirupsen/logrus"

	"go.goms.io/aks/AKSFlexNode/pkg/components/arc"
	"go.goms.io/aks/AKSFlexNode/pkg/components/azurevm"
	"go.goms.io/aks/AKSFlexNode/pkg/components/cni"
	"go.goms.io/aks/AKSFlexNode/pkg/components/containerd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kube_binaries"
	"go.goms.io/aks/AKSFlexNode/pkg/components/kubelet"
	"go.goms.io/aks/AKSFlexNode/pkg/components/npd"
	"go.goms.io/aks/AKSFlexNode/pkg/components/runc"
	"go.goms.io/aks/AKSFlexNode/pkg/components/services"
	"go.goms.io/aks/AKSFlexNode/pkg/components/system_configuration"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
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
	steps := []Executor{}

	// Use Azure VM installer on Azure VMs, Arc installer for non-Azure machines
	if utils.IsRunningOnAzureVM() {
		b.logger.Info("Detected Azure VM - using Azure VM installer")
		steps = append(steps, azurevm.NewInstaller(b.logger)) // Setup Azure VM
	} else {
		steps = append(steps, arc.NewInstaller(b.logger)) // Setup Arc
	}

	steps = append(steps,
		services.NewUnInstaller(b.logger),           // Stop kubelet before setup
		system_configuration.NewInstaller(b.logger), // Configure system (early)
		runc.NewInstaller(b.logger),                 // Install runc
		containerd.NewInstaller(b.logger),           // Install containerd
		kube_binaries.NewInstaller(b.logger),        // Install k8s binaries
		cni.NewInstaller(b.logger),                  // Setup CNI (after container runtime)
		kubelet.NewInstaller(b.logger),              // Configure kubelet service with Arc MSI auth
		services.NewInstaller(b.logger),             // Start services
		npd.NewInstaller(b.logger),                  // Install Node Problem Detector
	)

	return b.ExecuteSteps(ctx, steps, "bootstrap")
}

// Unbootstrap executes all cleanup steps sequentially (in reverse order of bootstrap)
func (b *Bootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	steps := []Executor{
		npd.NewUnInstaller(b.logger),                  // Uninstall Node Problem Detector
		services.NewUnInstaller(b.logger),             // Stop services first
		kubelet.NewUnInstaller(b.logger),              // Clean kubelet configuration
		cni.NewUnInstaller(b.logger),                  // Clean CNI configs
		kube_binaries.NewUnInstaller(b.logger),        // Uninstall k8s binaries
		containerd.NewUnInstaller(b.logger),           // Uninstall containerd binary
		runc.NewUnInstaller(b.logger),                 // Uninstall runc binary
		system_configuration.NewUnInstaller(b.logger), // Clean system settings
	}

	// Use Azure VM uninstaller on Azure VMs, Arc uninstaller for non-Azure machines
	if utils.IsRunningOnAzureVM() {
		steps = append(steps, azurevm.NewUnInstaller(b.logger)) // Cleanup Azure VM
	} else {
		steps = append(steps, arc.NewUnInstaller(b.logger)) // Uninstall Arc (after cleanup)
	}

	return b.ExecuteSteps(ctx, steps, "unbootstrap")
}
