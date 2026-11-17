package start

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentruntime"
	"github.com/Azure/AKSFlexNode/pkg/logger"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func NewCommand() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:     "start",
		Aliases: []string{"bootstrap"},
		Short:   "Bootstrap the node and start the agent service",
		Long:    "Install the systemd unit, bootstrap the AKS worker node, then enable and start the agent daemon through systemd.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config from %s: %w", configPath, err)
			}
			if err := daemon.ValidateAgentDeployment(cfg.Bootstrap.Isolation, agentdeployment.Containerized()); err != nil {
				return err
			}
			if err := runStart(cmd.Context(), cfg); err != nil {
				return err
			}

			fmt.Println()
			fmt.Println("AKS Flex Node agent service started successfully.")
			fmt.Println()
			fmt.Println("Next steps:")
			fmt.Println("  Check service status: systemctl status aks-flex-node-agent")
			fmt.Println("  View service logs:    journalctl -u aks-flex-node-agent -f")
			fmt.Println("  Stop agent:           systemctl stop aks-flex-node-agent")
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to configuration JSON file (required)")
	_ = cmd.MarkFlagRequired("config")

	return cmd
}

func runStart(ctx context.Context, cfg *config.Config) error {
	// In its container the agent keeps nothing on the host: the image carries
	// it, and its clients' credential plugin is the running binary.
	containerized := agentdeployment.Containerized()
	stateStore, err := daemon.NewFileStateStore()
	if err != nil {
		return err
	}
	applied, err := stateStore.Load(ctx)
	if err != nil {
		return err
	}
	if err := daemon.ValidateWorkerBootstrap(ctx, cfg, slog.Default(), applied, containerized); err != nil {
		return err
	}
	worker, err := agentruntime.New(cfg.Bootstrap.Isolation)
	if err != nil {
		return err
	}
	state, goal, reapply, err := selectBootstrap(cfg, applied)
	if err != nil {
		return err
	}
	logger := logger.CreateLogger(cfg.Agent.LogLevel, cfg.Agent.LogDir)
	if !containerized {
		if err := agentdeployment.MigrateHostRoot(logger); err != nil {
			return err
		}
		// Before any client is built: one that authenticates with an exec
		// credential runs the binary under the host root, which an install script
		// may have put in /usr/local/bin instead. Copying it first also fails a
		// host whose host root cannot run programs before the machine is
		// registered.
		if err := agentdeployment.PrepareHostRoot(ctx, logger); err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
		if err := agentdeployment.InstallHostBinary(ctx, logger); err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
	}
	start := time.Now()
	var machines aksmachine.MachineClient
	if !reapply {
		plugin, err := agentdeployment.CredentialPlugin()
		if err != nil {
			return fmt.Errorf("create AKS machine client: %w", err)
		}
		machines, err = aksmachine.NewMachineClient(cfg, logger, aksmachine.MachineClientOptions{
			CredentialPlugin: plugin,
		})
		if err != nil {
			return fmt.Errorf("create AKS machine client: %w", err)
		}
	}
	if err := registerBootstrap(ctx, logger, cfg, machines, &goal, state, reapply); err != nil {
		return err
	}

	input := agentruntime.Inputs{
		Config: cfg,
		Node: agentruntime.Node{
			Slot:   state.ActiveMachine,
			Config: state.NodeConfig,
		},
	}
	if !reapply {
		resolved, err := config.ResolveMachineGoalState(ctx, logger, cfg, state.ActiveMachine)
		if err != nil {
			return fmt.Errorf("bootstrap failed to resolve goal state: %w", err)
		}
		input.Node.Config = resolved.AgentConfig
		input.Node.GoalState = resolved.GoalState
		input.Archives = resolved.ContainerImageArchives
		if cfg.Bootstrap.Isolation == isolation.ModeNone || cfg.Bootstrap.Isolation == isolation.ModeContainer {
			state.NodeConfig = resolved.AgentConfig.DeepCopy()
		}
	}

	tasks := bootstrapTasks(cfg, logger, worker, input, reapply, daemon.SaveState(stateStore, state))
	if err := phases.ExecuteTask(ctx, logger, tasks); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	// The daemon removes it at startup too. Doing it here as well closes the
	// window in which an AgentUpgrade to an earlier release could find it.
	if !containerized {
		if err := agentdeployment.RemoveLegacySeed(logger); err != nil {
			logger.Warn("failed to remove the agent binary left in /usr/local/bin", "error", err)
		}
	}
	logger.Info("operation completed successfully", "operation", "bootstrap", "duration", time.Since(start))

	return nil
}

func selectBootstrap(cfg *config.Config, applied *daemon.State) (*daemon.State, aksmachine.GoalState, bool, error) {
	seed := daemon.SeededState(aksmachine.GoalState{})
	seed.Isolation = cfg.Bootstrap.Isolation
	state, reapply, err := daemon.BootstrapState(applied, seed, cfg)
	if err != nil {
		return nil, aksmachine.GoalState{}, false, err
	}
	if reapply {
		return state, aksmachine.GoalState{}, true, nil
	}
	goal, err := aksmachine.GoalStateFromConfig(cfg)
	if err != nil {
		return nil, aksmachine.GoalState{}, false, fmt.Errorf("build goal state from config: %w", err)
	}
	return state, goal, false, nil
}

func registerBootstrap(ctx context.Context, log *slog.Logger, cfg *config.Config, machines aksmachine.MachineClient, goal *aksmachine.GoalState, state *daemon.State, reapply bool) error {
	if reapply {
		// The caller's bootstrap goal is not authoritative for an installed worker.
		return nil
	}
	required := cfg.Agent.RequireMachineRegistration != nil && *cfg.Agent.RequireMachineRegistration
	if err := phases.ExecuteTask(ctx, log, aksmachine.EnsureMachine(machines, goal, required, log)); err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	state.AppliedSettingsVersion = goal.SettingsVersion
	state.AppliedKubernetesVersion = goal.KubernetesVersion

	return nil
}

func workerBootstrapTasks(log *slog.Logger, worker agentruntime.Runtime, input agentruntime.Inputs, reapply bool, save phases.Task) []phases.Task {
	if reapply {
		return []phases.Task{worker.Recover(log, input), worker.Restart(log, input)}
	}

	return []phases.Task{worker.Start(log, input), save}
}

func bootstrapTasks(cfg *config.Config, log *slog.Logger, worker agentruntime.Runtime, input agentruntime.Inputs, reapply bool, save phases.Task) phases.Task {
	var tasks []phases.Task
	if !reapply {
		tasks = append(tasks, daemon.SetupHost(cfg, log))
		if cfg.Bootstrap.Isolation == "" || cfg.Bootstrap.Isolation == isolation.ModeNSpawn {
			tasks = append(tasks, agentdeployment.EnsureNSpawnLifecycleUnits(log))
		}
	}
	tasks = append(tasks, workerBootstrapTasks(log, worker, input, reapply, save)...)
	tasks = append(tasks, agentdeployment.InstallService(cfg, log))
	return phases.Serial(log, tasks...)
}
