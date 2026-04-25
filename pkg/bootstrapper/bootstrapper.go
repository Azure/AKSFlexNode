package bootstrapper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
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
	log := b.logger
	cfg := b.cfg

	// Step 1: Enrich cluster config (fetch serverURL/caCertData from AKS for non-bootstrap-token modes).
	// This must run before we build the agent config because it populates ServerURL and CACertData.
	enrichTask := EnrichClusterConfig(cfg, log)
	if err := phases.ExecuteTask(ctx, log, enrichTask); err != nil {
		return failedResult("enrich-cluster-config", err), fmt.Errorf("bootstrap failed at step enrich-cluster-config: %w", err)
	}

	// Step 2: Convert FlexNode config to shared agent config and resolve goal states.
	agentCfg := config.ToAgentConfig(cfg, b.machineName)
	gs, err := goalstates.ResolveMachine(log, agentCfg, b.machineName)
	if err != nil {
		return failedResult("resolve-goal-state", err), fmt.Errorf("bootstrap failed to resolve goal state: %w", err)
	}

	// Step 3: Build the task tree and execute.
	taskList := []phases.Task{
		// Phase 1: host preparation
		host.InstallPackages(log),
		phases.Parallel(log,
			host.ConfigureOS(log),
			host.ConfigureNFTables(log),
			host.DisableDocker(log),
			host.DisableSwap(log),
		),

		// Azure-specific: install Arc (no-op if not enabled)
		InstallArc(cfg, log),

		// Phase 2: rootfs provisioning (nspawn workspace + parallel binary downloads)
		rootfs.Provision(log, gs.RootFS),

		// Azure-specific: download NPD
		DownloadNPD(cfg),

		// Copy the aks-flex-node binary into the rootfs so it is available
		// inside the nspawn container (needed for exec credential plugins
		// and useful for debugging).
		InstallBinary(gs.RootFS.MachineDir),

		// Write the default bridge CNI config into the rootfs. The shared
		// library installs CNI binaries but not a conflist; without one
		// kubelet stays NetworkNotReady.
		WriteCNIConfig(gs.RootFS.MachineDir),
	}

	taskList = append(taskList,
		// Phase 3: node start (configure + boot nspawn + start containerd + kubelet)
		nodestart.StartNode(log, gs.NodeStart),

		// Azure-specific: start NPD
		StartNPD(cfg),

		// Azure-specific: register this machine with the AKS Machines API.
		// TODO: enable once the Machines API is available in all target environments.
		// EnsureMachine(cfg, log),
	)

	tasks := phases.Serial(log, taskList...)

	start := time.Now()
	err = tasks.Do(ctx)

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
	log := b.logger

	// Unbootstrap uses best-effort semantics: continue even if steps fail.
	start := time.Now()
	var firstErr error

	steps := []phases.Task{
		nodestop.StopNode(log, b.machineName),
		reset.CleanupMachine(log, b.machineName),
		UninstallArc(log),
	}

	for _, step := range steps {
		if err := phases.ExecuteTask(ctx, log, step); err != nil {
			log.Warn("unbootstrap step failed (continuing)", "step", step.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	result := &ExecutionResult{
		Success:  firstErr == nil,
		Duration: time.Since(start),
	}
	if firstErr != nil {
		result.Error = firstErr.Error()
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
