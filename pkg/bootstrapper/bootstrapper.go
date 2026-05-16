package bootstrapper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/arc"
	"github.com/Azure/AKSFlexNode/pkg/cni"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
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
	machines    aksmachine.MachineClient
}

// New creates a new bootstrapper. machineName is the nspawn machine name
// (e.g. goalstates.NSpawnMachineKube1).
func New(
	cfg *config.Config,
	logger *slog.Logger,
	machineName string,
	machines aksmachine.MachineClient,
) *Bootstrapper {
	return &Bootstrapper{
		cfg:         cfg,
		logger:      logger,
		machineName: machineName,
		machines:    machines,
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
	gs, err := goalstates.ResolveMachine(b.logger, agentCfg, b.machineName, nil)
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

		// Parallel rootfs customisation: download NPD, copy the daemon binary,
		// and write the bridge CNI config into the machine rootfs.
		phases.Parallel(b.logger,
			npd.Download(b.cfg, gs.RootFS.MachineDir),
			daemon.InstallBinary(gs.RootFS.MachineDir),
			cni.WriteCNIConfig(gs.RootFS.MachineDir),
		),

		// Phase 3: node start (configure + boot nspawn + start containerd + kubelet)
		nodestart.StartNode(b.logger, gs.NodeStart),

		// Azure-specific: start NPD inside the nspawn container
		npd.Start(b.cfg, b.logger, gs.RootFS.MachineDir, b.machineName),

		daemon.NewSeedStateTask(b.machines, b.machineName),
		daemon.EnableAndStartServiceTask(b.logger),
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
