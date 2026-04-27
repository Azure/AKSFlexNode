package bootstrapper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/arc"
	"github.com/Azure/AKSFlexNode/pkg/cni"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Bootstrapper orchestrates the bootstrap and unbootstrap sequences using
// the shared unbounded agent library for Kubernetes operations and native
// task implementations for Azure-specific components (Arc, NPD).
type Bootstrapper struct {
	cfg         *config.Config
	logger      *slog.Logger
	machineName string
}

// New creates a new bootstrapper. machineName is the nspawn machine name
// (e.g. goalstates.NSpawnMachineKube1).
func New(
	cfg *config.Config,
	logger *slog.Logger,
	machineName string,
) *Bootstrapper {
	return &Bootstrapper{
		cfg:         cfg,
		logger:      logger,
		machineName: machineName,
	}
}

// Bootstrap executes all bootstrap steps to transform a bare VM into a
// Kubernetes worker node running inside an nspawn machine.
func (b *Bootstrapper) Bootstrap(ctx context.Context) (*ExecutionResult, error) {
	// Enrich cluster config (fetch serverURL/caCertData from AKS for non-bootstrap-token modes).
	// This must run before we build the agent config because it populates ServerURL and CACertData.
	if err := EnrichClusterConfig(ctx, b.cfg, b.logger); err != nil {
		return failedResult("enrich-cluster-config", err), fmt.Errorf("bootstrap failed at step enrich-cluster-config: %w", err)
	}

	// Convert FlexNode config to shared agent config and resolve goal states.
	agentCfg := config.ToAgentConfig(b.cfg, b.machineName)
	gs, err := goalstates.ResolveMachine(b.logger, agentCfg, b.machineName)
	if err != nil {
		return failedResult("resolve-goal-state", err), fmt.Errorf("bootstrap failed to resolve goal state: %w", err)
	}

	// Build the task tree and execute.
	bootstrapTasks := phases.Serial(b.logger,
		// Phase 1: host preparation
		host.InstallPackages(b.logger),
		phases.Parallel(b.logger,
			host.ConfigureOS(b.logger),
			host.ConfigureNFTables(b.logger),
			host.DisableDocker(b.logger),
			host.DisableSwap(b.logger),
			host.HardenAPT(b.logger),
			arc.InstallArc(b.cfg, b.logger),
		),

		// Phase 2: rootfs provisioning (nspawn workspace + parallel binary downloads)
		rootfs.Provision(b.logger, gs.RootFS),

		// Azure-specific: download NPD into the machine rootfs
		npd.Download(b.cfg, gs.RootFS.MachineDir),

		// Copy the aks-flex-node binary into the rootfs so it is available
		// inside the nspawn container (needed for exec credential plugins
		// and useful for debugging).
		InstallBinary(gs.RootFS.MachineDir),

		// Write the default bridge CNI config into the rootfs. The shared
		// library installs CNI binaries but not a conflist; without one
		// kubelet stays NetworkNotReady.
		cni.WriteCNIConfig(gs.RootFS.MachineDir),

		// Phase 3: node start (configure + boot nspawn + start containerd + kubelet)
		nodestart.StartNode(b.logger, gs.NodeStart),

		// Azure-specific: start NPD inside the nspawn container
		npd.Start(b.cfg, b.logger, gs.RootFS.MachineDir, b.machineName),

		// Azure-specific: register this machine with the AKS Machines API.
		// TODO: enable once the Machines API is available in all target environments.
		// aksmachine.EnsureMachine(b.cfg, b.logger),
	)

	start := time.Now()
	err = bootstrapTasks.Do(ctx)

	result := &ExecutionResult{
		Success:  err == nil,
		Duration: time.Since(start),
	}
	if err != nil {
		result.Error = err.Error()
		return result, fmt.Errorf("bootstrap failed: %w", err)
	}

	return result, nil
}

// Unbootstrap tears down the Kubernetes node and cleans up all resources.
func (b *Bootstrapper) Unbootstrap(ctx context.Context) (*ExecutionResult, error) {
	start := time.Now()

	unbootstrapTasks := phases.Serial(b.logger,
		nodestop.StopNode(b.logger, b.machineName),
		reset.CleanupMachine(b.logger, b.machineName),
	)

	err := unbootstrapTasks.Do(ctx)

	// Best-effort: attempt Arc uninstall regardless of earlier failures.
	if arcErr := phases.ExecuteTask(ctx, b.logger, arc.UninstallArc(b.logger)); arcErr != nil {
		b.logger.Warn("arc uninstall failed (continuing)", "error", arcErr)
		if err == nil {
			err = arcErr
		}
	}

	result := &ExecutionResult{
		Success:  err == nil,
		Duration: time.Since(start),
	}
	if err != nil {
		result.Error = err.Error()
	}

	return result, nil
}

// failedResult creates an ExecutionResult for an early failure.
func failedResult(step string, err error) *ExecutionResult {
	return &ExecutionResult{
		Success: false,
		Error:   fmt.Sprintf("step %s: %s", step, err),
	}
}
