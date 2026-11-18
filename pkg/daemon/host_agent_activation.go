package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// PreflightHostAgentActivation plans activation without changing the installed layout or units.
func PreflightHostAgentActivation(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationPlan, error) {
	if agentdeployment.Containerized() {
		return agentbinary.ActivationPlan{}, agentdeployment.ErrDirectActivationInContainer
	}

	service, paths, err := newFlexDaemonActivationService(log, agentdeployment.PlannedHostRoot)
	if err != nil {
		return agentbinary.ActivationPlan{}, err
	}

	return agentbinary.PreflightHostDaemonActivation(ctx, agentdeployment.HostAgentActivationOptions(paths, candidatePath), service)
}

// ActivateHostAgent activates a directly staged Flex agent binary. It uses the
// same lock and binary layout as MachineOperation upgrades.
func ActivateHostAgent(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationResult, error) {
	if agentdeployment.Containerized() {
		return agentbinary.ActivationResult{}, agentdeployment.ErrDirectActivationInContainer
	}
	if os.Geteuid() != 0 {
		return agentbinary.ActivationResult{}, fmt.Errorf("host agent upgrade requires root privileges")
	}
	if err := agentdeployment.MigrateHostRoot(log); err != nil {
		return agentbinary.ActivationResult{}, err
	}
	if err := agentdeployment.GuardActivationLegacySeed(ctx, log); err != nil {
		return agentbinary.ActivationResult{}, err
	}

	service, paths, err := newFlexDaemonActivationService(log, hostroot.Resolve)
	if err != nil {
		return agentbinary.ActivationResult{}, err
	}

	return agentbinary.ActivateHostDaemon(ctx, log, agentdeployment.HostAgentActivationOptions(paths, candidatePath), service)
}

type hostActivationService interface {
	Preflight(context.Context, string, agentdeployment.ServiceTargets) (agentbinary.ServicePlan, error)
	Prepare(context.Context, string) error
	Reload(context.Context) error
	Restart(context.Context) error
	WaitHealthyForBinary(context.Context, string) (string, error)
}

type flexDaemonActivationService struct {
	hostActivationService
	state       stateStore
	log         *slog.Logger
	configPath  string
	synchronize func(string, string) error
}

func newFlexDaemonActivationService(log *slog.Logger, resolveRoot func() string) (*flexDaemonActivationService, agentdeployment.UpgradePaths, error) {
	root := resolveRoot()
	state, err := NewFileStateStore()
	if err != nil {
		return nil, agentdeployment.UpgradePaths{}, err
	}

	service, paths := agentdeployment.NewHostActivationService(log, root, resolveRoot)
	return &flexDaemonActivationService{
		hostActivationService: service,
		state:                 state,
		log:                   log,
		configPath:            filepath.Join(config.ConfigDir, "config.json"),
		synchronize:           agentdeployment.SynchronizeAgentBinary,
	}, paths, nil
}

func (s *flexDaemonActivationService) Preflight(ctx context.Context, currentBinaryPath string) (agentbinary.ServicePlan, error) {
	state, err := s.state.Load(ctx)
	if err != nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("load applied worker state: %w", err)
	}

	cfg, err := config.LoadConfig(s.configPath)
	if err != nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("load host agent configuration: %w", err)
	}

	mode := cfg.Bootstrap.Isolation
	if state != nil {
		mode = state.Isolation
	}

	targets := agentdeployment.ServiceTargetsFromConfig(cfg, mode)
	return s.hostActivationService.Preflight(ctx, currentBinaryPath, targets)
}

func (s *flexDaemonActivationService) WaitHealthy(ctx context.Context, expectedBinaryPath string) error {
	healthCtx, cancel := context.WithTimeout(ctx, agentdeployment.HostAgentHealthTimeout)
	defer cancel()

	expected, err := s.WaitHealthyForBinary(healthCtx, expectedBinaryPath)
	if err != nil {
		return err
	}
	return s.synchronizeActiveWorker(healthCtx, expected)
}

func (s *flexDaemonActivationService) synchronizeActiveWorker(ctx context.Context, expected string) error {
	state, err := s.state.Load(ctx)
	if err != nil {
		return fmt.Errorf("load active worker state: %w", err)
	}
	if state == nil {
		s.log.Info("activated host agent without an active worker")
		return nil
	}
	if !validActiveMachine(state.ActiveMachine) {
		return fmt.Errorf("no valid active worker for agent activation")
	}
	if state.Isolation == isolation.ModeNone {
		return nil
	}

	helpers, err := agentUpgradeHelperPaths(s.log, state.Isolation)
	if err != nil {
		return fmt.Errorf("resolve active worker for agent activation: %w", err)
	}
	if err := s.synchronize(expected, helpers[state.ActiveMachine]); err != nil {
		return fmt.Errorf("synchronize worker credential helper: %w", err)
	}
	return nil
}

var _ agentbinary.DaemonService = (*flexDaemonActivationService)(nil)
