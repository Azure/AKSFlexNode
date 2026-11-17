package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/npd"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

const containerUpgradeSnapshot = "container-upgrade-config"

type containerRuntime struct {
	newNode       func(containerops.Params) *containerops.Node
	stageArchives func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task
}

func (r containerRuntime) node(log *slog.Logger, input Node) *containerops.Node {
	node := r.newNode(containerops.Params{
		Log:       log,
		Config:    input.Config,
		Slot:      input.Slot,
		GoalState: input.GoalState,
		Overrides: containerops.Overrides{
			HelperPath: filepath.Join(hostroot.Path, "bin/aks-flex-node"),
		},
	})
	// Prepare installs runc; preflight must still reject an existing bad binary.
	node.Backend().RuntimeProvisioned = true
	return node
}

func (r containerRuntime) Worker(log *slog.Logger, input Node) (WorkerContext, error) {
	node := r.node(log, input)
	gs, err := node.GoalState()
	if err != nil {
		return WorkerContext{}, fmt.Errorf("resolve OCI worker: %w", err)
	}
	return WorkerContext{
		Root: gs.RootFS.Root(),
		Exec: func(ctx context.Context, args ...string) (string, error) {
			return gs.NodeStart.Exec(ctx, log, args...)
		},
		KubeletUnit: "kubelet.service",
		Mode:        isolation.ModeContainer,
	}, nil
}

func containerArtifacts(ctx context.Context, log *slog.Logger, cfg *config.Config, gs *goalstates.MachineGoalState) error {
	return phases.Parallel(log,
		&installBinaryTask{root: gs.RootFS.Root()},
		npd.Download(log, cfg, gs.RootFS.Root()),
	).Do(ctx)
}

// Upgrade owns preparation and activation, so install guest artifacts in its
// unpack hook before it can start kubelet.
func bindContainerArtifacts(node *containerops.Node, log *slog.Logger, cfg *config.Config) (*goalstates.MachineGoalState, error) {
	gs, err := node.GoalState()
	if err != nil {
		return nil, err
	}
	backend := node.Backend()
	unpack := backend.Unpack
	backend.Unpack = func(ctx context.Context, spec isolation.NodeSpec, dir string) error {
		if err := unpack(ctx, spec, dir); err != nil {
			return err
		}
		return containerArtifacts(ctx, log, cfg, gs)
	}
	return gs, nil
}

func (r containerRuntime) Start(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("start-oci-worker", func(ctx context.Context) error {
		node := r.node(log, input.Node)
		if input.Previous == nil {
			if err := node.Backend().Preflight(); err != nil {
				return fmt.Errorf("preflight OCI worker: %w", err)
			}
		}
		if err := r.stageArchives(log, input.Archives).Do(ctx); err != nil {
			return fmt.Errorf("stage container image archives: %w", err)
		}
		if input.Previous != nil {
			gs, err := bindContainerArtifacts(node, log, input.Config)
			if err != nil {
				return err
			}
			if err := node.Upgrade(ctx, r.node(log, *input.Previous)); err != nil {
				return fmt.Errorf("upgrade OCI worker: %w", err)
			}
			// SDK rollback ends when Upgrade returns, before this NPD action.
			return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx)
		}

		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		if _, err := node.Prepare(ctx); err != nil {
			return fmt.Errorf("prepare OCI worker: %w", err)
		}
		if err := containerArtifacts(ctx, log, input.Config, gs); err != nil {
			return fmt.Errorf("install OCI worker artifacts: %w", err)
		}
		containerdChanged, kubeletChanged, err := node.Configure(ctx)
		if err != nil {
			return fmt.Errorf("configure OCI worker: %w", err)
		}
		if err := node.Start(ctx, false, containerdChanged, kubeletChanged); err != nil {
			return fmt.Errorf("start OCI worker: %w", err)
		}
		return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx)
	})
}

func (r containerRuntime) Restart(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("restart-oci-worker", func(ctx context.Context) error {
		node := r.node(log, input.Node)
		if err := node.Restart(ctx); err != nil {
			return fmt.Errorf("restart OCI worker: %w", err)
		}
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		return npd.Start(log, input.Config, gs.RootFS.Root(), gs.NodeStart, isolation.ModeContainer).Do(ctx)
	})
}

func (r containerRuntime) PrepareUpgrade(*slog.Logger, Inputs) phases.Task {
	return hostnode.Func("prepare-oci-upgrade", func(context.Context) error { return nil })
}

func (r containerRuntime) Recover(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("rewrite-oci-unit", func(ctx context.Context) error {
		// The SDK has no durable OCI upgrade recovery.
		return r.node(log, input.Node).RewriteUnit(ctx)
	})
}

// RewriteContainerHostRoot updates the network helper and supervisor after a host-root move.
func RewriteContainerHostRoot(ctx context.Context, log *slog.Logger, input Node) error {
	runtime := containerRuntime{newNode: containerops.New}
	node := runtime.node(log, input)
	if _, err := os.Stat(filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSNetworkUnit)); err == nil {
		gs, err := node.GoalState()
		if err != nil {
			return err
		}
		if err := nodestart.WriteLocalDNSNetworkFiles(gs.NodeStart); err != nil {
			return fmt.Errorf("rewrite LocalDNS network files: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := node.RewriteUnit(ctx); err != nil {
		return fmt.Errorf("rewrite OCI supervisor: %w", err)
	}
	return nil
}

func (r containerRuntime) Retire(log *slog.Logger, input Inputs) phases.Task {
	return hostnode.Func("retire-oci-worker", func(ctx context.Context) error {
		if err := r.node(log, *input.Previous).Destroy(ctx); err != nil {
			log.Warn("could not remove superseded OCI slot; next repave retries", "error", err)
		}
		return nil
	})
}

func (containerRuntime) PlanReset(log *slog.Logger) (ResetPlan, error) { return planReset(log) }

type ociTeardown struct {
	sdk *containerops.Teardown
}

func (t *ociTeardown) Present() bool { return t.sdk.Present() }

func (t *ociTeardown) Stop() phases.Task { return t.sdk.Stop() }

func (t *ociTeardown) Destroy() phases.Task { return containerTeardown(t.sdk) }

func containerTeardown(teardown *containerops.Teardown) phases.Task {
	return hostnode.Func("reset-oci-node", func(ctx context.Context) (err error) {
		defer func() {
			if err == nil {
				return
			}
			// Destroy can remove all slots before returning an error. Retain the
			// existing SDK ownership trace so a no-State CLI retry still selects OCI.
			if keepErr := os.MkdirAll(teardown.Layout.RootfsDir(""), 0o750); keepErr != nil {
				err = errors.Join(err, fmt.Errorf("retain OCI reset ownership: %w", keepErr))
			}
		}()

		// Destroy stops existing supervisors and checks that every container is
		// gone, including slots whose unit was never written or was removed.
		if err := teardown.Destroy().Do(ctx); err != nil {
			return fmt.Errorf("tear down OCI worker: %w", err)
		}
		if err := teardown.RemoveConfig().Do(ctx); err != nil {
			return fmt.Errorf("remove OCI worker configuration: %w", err)
		}
		return removeContainerUpgradeArtifact(teardown.Root)
	})
}

// ContainerUpgradeArtifactPresent detects an interrupted SDK upgrade snapshot.
func ContainerUpgradeArtifactPresent() (bool, error) {
	path := filepath.Join(goalstates.AgentConfigDir, containerUpgradeSnapshot)
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect OCI upgrade artifact %s: %w", path, err)
	}
	return false, nil
}

// CleanupContainerUpgradeArtifact removes an orphaned snapshot without touching worker paths.
func CleanupContainerUpgradeArtifact() phases.Task {
	return hostnode.Func("remove-oci-upgrade-artifact", func(context.Context) error {
		return removeContainerUpgradeArtifact("")
	})
}

func removeContainerUpgradeArtifact(hostRoot string) error {
	path := filepath.Join(hostRoot, goalstates.AgentConfigDir, containerUpgradeSnapshot)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove OCI upgrade artifact %s: %w", path, err)
	}
	return nil
}
