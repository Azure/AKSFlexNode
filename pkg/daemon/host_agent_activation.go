package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
)

const (
	hostAgentHealthTimeout  = 30 * time.Second
	hostAgentStableDuration = 3 * time.Second
	hostAgentHealthPoll     = 250 * time.Millisecond
)

// PreflightHostAgentActivation validates a directly staged Flex agent binary
// and returns the shared activation plan without changing host state.
func PreflightHostAgentActivation(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationPlan, error) {
	service, paths, err := newFlexDaemonActivationService(log)
	if err != nil {
		return agentbinary.ActivationPlan{}, err
	}
	return agentbinary.PreflightHostDaemonActivation(ctx, hostAgentActivationOptions(paths, candidatePath), service)
}

// ActivateHostAgent activates a directly staged Flex agent binary. It uses the
// same lock and binary layout as MachineOperation upgrades.
func ActivateHostAgent(ctx context.Context, log *slog.Logger, candidatePath string) (agentbinary.ActivationResult, error) {
	if os.Geteuid() != 0 {
		return agentbinary.ActivationResult{}, fmt.Errorf("host agent upgrade requires root privileges")
	}
	service, paths, err := newFlexDaemonActivationService(log)
	if err != nil {
		return agentbinary.ActivationResult{}, err
	}
	return agentbinary.ActivateHostDaemon(ctx, log, hostAgentActivationOptions(paths, candidatePath), service)
}

func hostAgentActivationOptions(paths agentUpgradePaths, candidatePath string) agentbinary.ActivationOptions {
	return agentbinary.ActivationOptions{
		Layout:        paths.layout(),
		CandidatePath: candidatePath,
		BinaryMode:    agentUpgradeBinaryMode,
		LockPath:      agentUpgradeLockPath,
	}
}

type flexDaemonActivationService struct {
	log              *slog.Logger
	paths            agentUpgradePaths
	state            stateStore
	systemdDir       string
	recoveryScript   string
	inspectService   func(context.Context, *slog.Logger, string) (bool, error)
	serviceWasActive bool
}

func newFlexDaemonActivationService(log *slog.Logger) (*flexDaemonActivationService, agentUpgradePaths, error) {
	if log == nil {
		log = slog.Default()
	}
	state, err := NewFileStateStore()
	if err != nil {
		return nil, agentUpgradePaths{}, err
	}
	paths := defaultAgentUpgradePaths()
	return &flexDaemonActivationService{
		log:            log,
		paths:          paths,
		state:          state,
		systemdDir:     systemdSystemDir,
		recoveryScript: recoveryScriptPath,
		inspectService: inspectAgentServiceActive,
	}, paths, nil
}

func (s *flexDaemonActivationService) Preflight(ctx context.Context, currentBinaryPath string) (agentbinary.ServicePlan, error) {
	inspectService := s.inspectService
	if inspectService == nil {
		inspectService = inspectAgentServiceActive
	}
	serviceWasActive, err := inspectService(ctx, s.log, ServiceUnitName)
	if err != nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("inspect agent service state: %w", err)
	}
	s.serviceWasActive = serviceWasActive
	if _, err := os.Stat(s.paths.SignalPath); err == nil {
		return agentbinary.ServicePlan{}, fmt.Errorf("AgentUpgrade MachineOperation signal exists at %s", s.paths.SignalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return agentbinary.ServicePlan{}, fmt.Errorf("inspect AgentUpgrade MachineOperation signal: %w", err)
	}
	for _, asset := range desiredAgentServiceAssets(s.paths, s.systemdDir, s.recoveryScript, currentBinaryPath) {
		actual, err := os.ReadFile(asset.path)
		if errors.Is(err, os.ErrNotExist) || err == nil && !bytes.Equal(actual, asset.content) {
			return agentbinary.ServicePlan{
				UpdateRequired: true,
				Description:    "install or update AKS Flex Node agent systemd assets",
			}, nil
		}
		if err != nil {
			return agentbinary.ServicePlan{}, fmt.Errorf("read daemon asset %s: %w", asset.path, err)
		}
	}
	return agentbinary.ServicePlan{Description: "AKS Flex Node agent systemd assets are current"}, nil
}

func inspectAgentServiceActive(ctx context.Context, log *slog.Logger, service string) (bool, error) {
	state, err := utilexec.OutputCmdAt(ctx, log, slog.LevelDebug, "systemctl", "show", "--property=ActiveState", "--value", service)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(state) {
	case "inactive":
		return false, nil
	case "active", "activating", "reloading", "deactivating", "failed":
		return true, nil
	default:
		return false, fmt.Errorf("unexpected ActiveState %q for %s", state, service)
	}
}

func (s *flexDaemonActivationService) Prepare(_ context.Context, currentBinaryPath string) error {
	return writeAgentServiceAssets(s.paths, s.systemdDir, s.recoveryScript, currentBinaryPath)
}

func (s *flexDaemonActivationService) Reload(ctx context.Context) error {
	if err := utilexec.ReloadSystemd(ctx, s.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

func (s *flexDaemonActivationService) Restart(ctx context.Context) error {
	if !s.serviceWasActive {
		s.log.Info("leaving inactive host agent service stopped after activation")
		return nil
	}
	if err := utilexec.RunCmd(ctx, s.log, utilexec.Systemctl(), "restart", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", ServiceUnitName, err)
	}
	return nil
}

// WaitHealthy preserves an inactive service during reset/reinstall. Otherwise,
// it synchronizes the active nspawn exec credential only after systemd is
// stably running the expected host binary. Shared rollback calls this again
// with last-good.
func (s *flexDaemonActivationService) WaitHealthy(ctx context.Context, expectedBinaryPath string) error {
	healthCtx, cancel := context.WithTimeout(ctx, hostAgentHealthTimeout)
	defer cancel()
	expected, err := filepath.EvalSymlinks(expectedBinaryPath)
	if err != nil {
		return fmt.Errorf("resolve expected daemon binary: %w", err)
	}
	if !s.serviceWasActive {
		return s.synchronizeActiveNspawn(healthCtx, expected)
	}
	var healthySince time.Time
	ticker := time.NewTicker(hostAgentHealthPoll)
	defer ticker.Stop()
	for {
		healthy, checkErr := s.isExpectedDaemonActive(healthCtx, expected)
		if checkErr == nil && healthy {
			if healthySince.IsZero() {
				healthySince = time.Now()
			} else if time.Since(healthySince) >= hostAgentStableDuration {
				return s.synchronizeActiveNspawn(healthCtx, expected)
			}
		} else {
			healthySince = time.Time{}
		}
		select {
		case <-healthCtx.Done():
			if checkErr != nil {
				return fmt.Errorf("daemon did not become healthy: %w", checkErr)
			}
			return fmt.Errorf("daemon did not execute expected binary %s: %w", expected, healthCtx.Err())
		case <-ticker.C:
		}
	}
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
	if !validNspawnMachine(state.ActiveMachine) {
		return fmt.Errorf("no valid active nspawn machine for agent activation")
	}
	if err := synchronizeNspawnAgentBinary(expected, state.ActiveMachine); err != nil {
		return fmt.Errorf("synchronize active nspawn agent binary: %w", err)
	}
	return nil
}

func (s *flexDaemonActivationService) isExpectedDaemonActive(ctx context.Context, expected string) (bool, error) {
	output, err := utilexec.OutputCmd(ctx, s.log, "systemctl", "show", "--property", "MainPID", "--value", ServiceUnitName)
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("invalid daemon MainPID %q", output)
	}
	running, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return false, err
	}
	return running == expected, nil
}

var _ agentbinary.DaemonService = (*flexDaemonActivationService)(nil)
