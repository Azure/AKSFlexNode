package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// PreflightHostAgentActivation validates the applied mode against a private
// candidate and plans activation without changing the installed layout or units.
func PreflightHostAgentActivation(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationPlan, error) {
	if log == nil {
		return agentbinary.ActivationPlan{}, fmt.Errorf("logger is nil")
	}

	if agentdeployment.Containerized() {
		return agentbinary.ActivationPlan{}, agentdeployment.ErrDirectActivationInContainer
	}

	candidate, cleanup, err := agentdeployment.SnapshotCandidate(candidatePath)
	if err != nil {
		return agentbinary.ActivationPlan{}, err
	}
	defer cleanup()

	service, paths, err := newFlexDaemonActivationService(log, agentdeployment.PlannedHostRoot, candidate)
	if err != nil {
		return agentbinary.ActivationPlan{}, err
	}
	plan, err := agentbinary.PreflightHostDaemonActivation(ctx, agentdeployment.HostAgentActivationOptions(paths, candidate), service)
	// Report the caller's path rather than the temporary input that is about to be removed.
	plan.CandidatePath = candidatePath
	return plan, err
}

// ActivateHostAgent activates a directly staged Flex agent binary. It uses the
// same lock and binary layout as MachineOperation upgrades.
func ActivateHostAgent(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationResult, error) {
	if log == nil {
		return agentbinary.ActivationResult{}, fmt.Errorf("logger is nil")
	}

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
	candidate, cleanup, err := agentdeployment.SnapshotCandidate(candidatePath)
	if err != nil {
		return agentbinary.ActivationResult{}, err
	}
	defer cleanup()

	service, paths, err := newFlexDaemonActivationService(log, hostroot.Resolve, candidate)
	if err != nil {
		return agentbinary.ActivationResult{}, err
	}
	return agentbinary.ActivateHostDaemon(ctx, log, agentdeployment.HostAgentActivationOptions(paths, candidate), service)
}

type flexDaemonActivationService struct {
	*agentdeployment.HostActivationService
	state         stateStore
	log           *slog.Logger
	candidatePath string
	synchronize   func(string, string) error
}

func newFlexDaemonActivationService(log *slog.Logger, resolveRoot func() string, candidatePath string) (*flexDaemonActivationService, agentdeployment.UpgradePaths, error) {
	if log == nil {
		return nil, agentdeployment.UpgradePaths{}, fmt.Errorf("logger is nil")
	}

	root := resolveRoot()
	state, err := NewFileStateStore()
	if err != nil {
		return nil, agentdeployment.UpgradePaths{}, err
	}
	service, paths, err := agentdeployment.NewHostActivationService(log, root, resolveRoot)
	if err != nil {
		return nil, agentdeployment.UpgradePaths{}, err
	}
	return &flexDaemonActivationService{
		HostActivationService: service,
		state:                 state,
		log:                   log,
		candidatePath:         candidatePath,
		synchronize:           agentdeployment.SynchronizeAgentBinary,
	}, paths, nil
}

func (s *flexDaemonActivationService) Preflight(ctx context.Context, currentBinaryPath string) (agentbinary.ServicePlan, error) {
	state, err := s.state.Load(ctx)
	if err != nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("load applied worker state: %w", err)
	}
	if state != nil {
		if err := agentdeployment.CheckIsolation(ctx, s.candidatePath, state.Isolation); err != nil {
			return agentbinary.ServicePlan{}, err
		}
		s.WorkerMode = state.Isolation
	}

	return s.HostActivationService.Preflight(ctx, currentBinaryPath)
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
