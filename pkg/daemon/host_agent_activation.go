package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

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
	service, paths, err := newFlexDaemonActivationService(log, hostroot.Resolve)
	if err != nil {
		return agentbinary.ActivationResult{}, err
	}
	return agentbinary.ActivateHostDaemon(ctx, log, agentdeployment.HostAgentActivationOptions(paths, candidatePath), service)
}

type flexDaemonActivationService struct {
	*agentdeployment.HostActivationService
	state       stateStore
	log         *slog.Logger
	synchronize func(string, string) error
}

func newFlexDaemonActivationService(log *slog.Logger, resolveRoot func() string) (*flexDaemonActivationService, agentdeployment.UpgradePaths, error) {
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
		synchronize:           agentdeployment.SynchronizeNspawnAgentBinary,
	}, paths, nil
}

func (s *flexDaemonActivationService) WaitHealthy(ctx context.Context, expectedBinaryPath string) error {
	healthCtx, cancel := context.WithTimeout(ctx, agentdeployment.HostAgentHealthTimeout)
	defer cancel()

	expected, err := s.WaitHealthyForBinary(healthCtx, expectedBinaryPath)
	if err != nil {
		return err
	}
	return s.synchronizeActiveNspawn(healthCtx, expected)
}

func (s *flexDaemonActivationService) synchronizeActiveNspawn(ctx context.Context, expected string) error {
	state, err := s.state.Load(ctx)
	if err != nil {
		return fmt.Errorf("load active nspawn state: %w", err)
	}
	if state == nil {
		s.log.Info("activated host agent without active nspawn synchronization")
		return nil
	}
	if !validActiveMachine(state.ActiveMachine) {
		return fmt.Errorf("no valid active nspawn machine for agent activation")
	}
	if err := s.synchronize(expected, state.ActiveMachine); err != nil {
		return fmt.Errorf("synchronize active nspawn agent binary: %w", err)
	}
	return nil
}

var _ agentbinary.DaemonService = (*flexDaemonActivationService)(nil)
