package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/yurthub"
	"github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

const (
	agentUpgradeStartupStability = 3 * time.Second
	agentUpgradePublishRetry     = 10 * time.Second
)

// Run starts the machine-driven daemon loop.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	// controller-runtime intentionally emits a warning and drops its internal
	// logs unless the process explicitly configures its global logger.
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	// In its container the agent keeps nothing on the host to migrate, install,
	// or remove: the image carries it.
	containerized := agentdeployment.Containerized()
	if err := ValidateAgentDeployment(cfg.Bootstrap.Isolation, containerized); err != nil {
		return err
	}
	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	applied, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if applied != nil && applied.Isolation != cfg.Bootstrap.Isolation {
		return fmt.Errorf("worker isolation change requires reset")
	}
	if (cfg.Bootstrap.Isolation == isolation.ModeNone || cfg.Bootstrap.Isolation == isolation.ModeContainer) && applied == nil {
		return fmt.Errorf("applied %s worker state is missing", cfg.Bootstrap.Isolation)
	}
	worker, err := agentruntime.New(cfg.Bootstrap.Isolation)
	if err != nil {
		return err
	}

	// After an AgentUpgrade from a release that predates the host root, this is
	// the first time the new agent runs on the host.
	if !containerized {
		if err := agentdeployment.MigrateHostRoot(log); err != nil {
			return err
		}
	}

	// Existing direct-file installations may predate the recovery units. Keep
	// the binary layout and systemd rollback assets converged on every startup,
	// except while an AgentUpgrade is pending; see agentdeployment.EnsureDaemonServiceAssets.
	if !containerized {
		if err := agentdeployment.InstallHostBinary(ctx, log); err != nil {
			return err
		}
	}
	convergeHeldAssets, err := ensureHostInstall(ctx, log, cfg, containerized)
	if err != nil {
		return err
	}
	// After the units are converged, so nothing names the copy it removes. While
	// an AgentUpgrade is pending they are the last-good version's, which name it
	// only on a host not yet moved off /usr/local, where nothing is removed.
	if !containerized {
		if err := agentdeployment.RemoveDaemonLegacySeed(log); err != nil {
			log.Warn("failed to remove the agent binary left in /usr/local/bin", "error", err)
		}
	}
	if cfg.Bootstrap.Isolation == isolation.ModeNone || cfg.Bootstrap.Isolation == isolation.ModeContainer {
		input := agentruntime.Inputs{
			Config: cfg,
			Node: agentruntime.Node{
				Slot:   applied.ActiveMachine,
				Config: applied.NodeConfig,
			},
		}
		if err := worker.Recover(log, input).Do(ctx); err != nil {
			return fmt.Errorf("recover worker at startup: %w", err)
		}
	}
	// In its container, the agent's units and its binary in the active machine converge here,
	// before the daemon builds its clients, as a host's install does above: a daemon whose
	// credentials fail still leaves them as its image has them.
	if containerized {
		convergence := agentdeployment.NewContainerConvergence(log)
		held, err := convergeContainerStartup(ctx, store, convergence)
		if err != nil {
			return err
		}
		if held {
			convergeHeldAssets = convergence.WriteUnits
		}
	}
	if cfg.UsesKubeconfigCredentials() {
		if err := retireDaemonCredentials(); err != nil {
			return err
		}
	}
	restCfg, stopCredentials, err := daemonRESTConfig(ctx, cfg)
	if err != nil {
		return err
	}
	defer stopCredentials()
	machines, err := aksmachine.NewMachineClient(cfg, log, aksmachine.MachineClientOptions{
		KubernetesRESTConfig: restCfg,
	})
	if err != nil {
		return fmt.Errorf("create AKS machine client: %w", err)
	}
	nodeName := cfg.Agent.NodeName
	// TODO: use the ARM machine resource name once the AKS RP Machine API contract is defined.
	aksMachineName := nodeName
	mgr, err := ctrl.NewManager(restCfg, manager.Options{
		Scheme: newScheme(),
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		Cache: ctrlcache.Options{
			ByObject: map[client.Object]ctrlcache.ByObject{
				&corev1.Node{}: {
					Field: fields.OneTermEqualSelector("metadata.name", nodeName),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create daemon manager: %w", err)
	}
	var deploymentUpgrade agentdeployment.UpgradeExecutor
	if !containerized {
		helpers, err := agentUpgradeHelperPaths(log, cfg.Bootstrap.Isolation)
		if err != nil {
			return err
		}
		deploymentUpgrade = agentdeployment.NewHostUpgrade(log, cfg.Bootstrap.Isolation, helpers)
	}
	instanceID, err := newDaemonInstanceID()
	if err != nil {
		return fmt.Errorf("create daemon instance ID: %w", err)
	}
	if containerized {
		deploymentUpgrade = agentdeployment.NewContainerUpgrade(log)
	}
	upgrades := &agentUpgradeCoordinator{
		UpgradeExecutor: deploymentUpgrade,
		state:           store,
		instanceID:      instanceID,
	}
	directClient, err := client.New(restCfg, client.Options{Scheme: newScheme()})
	if err != nil {
		return fmt.Errorf("create direct Kubernetes client: %w", err)
	}
	// Bound Node requests so a stalled API cannot hold the serialized queue.
	hubConfig := rest.CopyConfig(restCfg)
	hubConfig.Timeout = 30 * time.Second
	hubClient, err := kubernetes.NewForConfig(hubConfig)
	if err != nil {
		return fmt.Errorf("create hub node client: %w", err)
	}
	hub := yurthub.NewReconciler(log, nodeName, cfg.Azure.TargetAgentPoolName, cfg.APIServerURL(), hubClient.CoreV1().Nodes(), time.Duration(cfg.Agent.MachineReconcileInterval))
	repaves, err := newRepaveReconciler(repaveReconcilerOptions{
		Log:                      log,
		Machines:                 machines,
		Client:                   mgr.GetClient(),
		Runtime:                  worker,
		State:                    store,
		Config:                   cfg,
		Refresher:                bootstrapDataRefresherForConfig(cfg),
		HookUnits:                agentdeployment.EnsureNSpawnLifecycleUnits,
		Uninstall:                agentdeployment.UninstallService,
		ClearConfig:              config.RemoveRuntimeDirs(log),
		NodeName:                 nodeName,
		MachineReconcileInterval: time.Duration(cfg.Agent.MachineReconcileInterval),
		Hub:                      hub,
	})
	if err != nil {
		return err
	}
	machineOperations, err := machineOperationReconciler(machineOperationReconcilerOptions{
		Client:               mgr.GetClient(),
		Reader:               mgr.GetAPIReader(),
		Log:                  log,
		NodeName:             nodeName,
		AKSMachineName:       aksMachineName,
		MachineOperationMode: cfg.Agent.MachineOperationMode,
		Runtime:              worker,
		State:                store,
		Config:               cfg,
		Uninstall:            agentdeployment.UninstallService,
		ClearConfig:          config.RemoveRuntimeDirs(log),
		Resolve:              config.ResolveMachineGoalState,
		AgentUpgrade:         upgrades,
	})
	if err != nil {
		return err
	}
	gate := newStartupGate()
	// The manager stops early, with a cause, when the daemon may not take work
	// and is not replaced in time; see holdForHostRoot.
	mgrCtx, stopManager := context.WithCancelCause(ctx)
	defer stopManager(nil)
	if err := daemon.SetupController(
		"aks-flex-node-daemon",
		mgr,
		gatedMachineOperationReconciler{delegate: machineOperations, gate: gate},
		gatedRepaveReconciler{delegate: repaves, gate: gate},
	); err != nil {
		return fmt.Errorf("setup daemon controller: %w", err)
	}

	go func() {
		// Keep host-mutating reconciliation gated until the candidate has reached
		// cache readiness and remained alive for a bounded stability interval.
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(agentUpgradeStartupStability):
		}
		openGateAfterAgentUpgradeJudgment(
			ctx,
			log,
			func(ctx context.Context) error {
				return publishAndClearAgentUpgradeSignal(ctx, log, directClient, upgrades)
			},
			upgrades.SignalStore().Read,
			convergeHeldAssets,
			func() {
				// After the upgrade is reported, so a move never runs
				// before this binary has shown it can start, and so the signal
				// it waits for is gone. In its container the agent has no
				// install on the host to move.
				if hostUpgrades, ok := deploymentUpgrade.(*agentdeployment.HostUpgrade); ok {
					restarted := reconcileHostRootUnderLock(ctx, log, cfg, store, hostUpgrades)
					unfinished, err := agentdeployment.HostRootMoveUnfinishedHere()
					if err != nil {
						log.Warn("could not tell whether the move to the host root has finished; holding as if it has not", "error", err)
						unfinished = true
					}
					if reason := holdForHostRoot(restarted, unfinished, hostUpgrades.Paths(), agentdeployment.DefaultUpgradePaths()); reason != "" {
						// The gate stays closed; the next process opens it.
						if err := awaitReplacement(ctx, log, reason, hostRootRestartWait); err != nil {
							stopManager(err)
						}
						return
					}
				}
				gate.open()
			},
			agentUpgradePublishRetry,
		)
	}()

	err = mgr.Start(mgrCtx)
	repaves.log.Info("daemon shutting down")
	// A daemon that was told to stop exits as it always has. One that stopped
	// itself waiting to be replaced exits with an error, so Restart= starts it.
	if cause := context.Cause(mgrCtx); err == nil && ctx.Err() == nil && cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}
