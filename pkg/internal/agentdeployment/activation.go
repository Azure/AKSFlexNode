package agentdeployment

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
	"github.com/Azure/unbounded/pkg/agent/hostroot"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

const (
	HostAgentHealthTimeout  = 30 * time.Second
	hostAgentStableDuration = 3 * time.Second
	hostAgentHealthPoll     = 250 * time.Millisecond
)

// ErrDirectActivationInContainer refuses binary activation in an image deployment.
var ErrDirectActivationInContainer = errors.New("direct activation is not available in the agent's container; " +
	"upgrade it with an AgentUpgrade that names an image")

func HostAgentActivationOptions(paths UpgradePaths, candidatePath string) agentbinary.ActivationOptions {
	return agentbinary.ActivationOptions{
		Layout:        paths.Layout(),
		CandidatePath: candidatePath,
		BinaryMode:    upgradeBinaryMode,
		LockPath:      upgradeLockPath,
	}
}

type HostActivationService struct {
	// WorkerMode overrides installed service detection when applied state exists.
	WorkerMode isolation.Mode
	log        *slog.Logger
	// root is the host root paths were built under, and resolveRoot what
	// resolved it; see Preflight. A nil resolveRoot skips the check.
	root        string
	resolveRoot func() string
	// moveUnfinished reports a move to the host root that has not finished;
	// see Preflight. A nil moveUnfinished skips the check.
	moveUnfinished   func() (bool, error)
	paths            UpgradePaths
	systemdDir       string
	recoveryScript   string
	serviceOptions   agentServiceOptions
	inspectService   func(context.Context, *slog.Logger, string) (bool, error)
	serviceWasActive bool
}

func NewHostActivationService(log *slog.Logger, root string, resolveRoot func() string) (*HostActivationService, UpgradePaths, error) {
	if log == nil {
		log = slog.Default()
	}
	paths := UpgradePathsUnder(root)
	serviceOptions, err := installedAgentServiceOptions(SystemdSystemDir)
	if err != nil {
		return nil, UpgradePaths{}, err
	}
	return &HostActivationService{
		log:            log,
		root:           root,
		resolveRoot:    resolveRoot,
		moveUnfinished: HostRootMoveUnfinishedHere,
		paths:          paths,
		systemdDir:     SystemdSystemDir,
		recoveryScript: RecoveryScriptPathUnder(root),
		serviceOptions: serviceOptions,
		inspectService: inspectAgentServiceActive,
	}, paths, nil
}

// Preflight checks the service before an activation switches anything. In an
// activation it runs under the activation lock.
//
// The layout was built from the host root before the lock was held. A daemon
// that moves the host from /usr/local to the host root holds the same lock, and
// may have moved it in between. An activation that went ahead would stage the
// candidate under /usr/local and point the units back there, and the move, whose
// copy still names the previous binary, would then point them at its copy and
// remove /usr/local, leaving the host on the previous binary. Checking here,
// under the lock, refuses it instead; a retry builds the layout from the root
// as it is now.
//
// A move whose first pass renamed its copy into place just before the layout
// was built leaves the root unchanged, but has not finished, so it is checked
// here too, as guardLegacySeed checks it before the lock is held.
func (s *HostActivationService) Preflight(ctx context.Context, currentBinaryPath string) (agentbinary.ServicePlan, error) {
	if s.resolveRoot != nil {
		if current := s.resolveRoot(); current != s.root {
			return agentbinary.ServicePlan{}, fmt.Errorf("the agent's host root moved from %s to %s while the activation was prepared; retry", s.root, current)
		}
	}
	if s.moveUnfinished != nil {
		if moving, err := s.moveUnfinished(); err != nil {
			return agentbinary.ServicePlan{}, fmt.Errorf("inspect the host root: %w", err)
		} else if moving {
			return agentbinary.ServicePlan{}, fmt.Errorf("the move of the agent's files to %s has not finished; restart %s, which finishes it, then retry",
				hostroot.Path, ServiceUnitName)
		}
	}
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
	if s.WorkerMode != "" {
		s.serviceOptions.OmitMachinesTarget = s.WorkerMode == isolation.ModeNone || s.WorkerMode == isolation.ModeContainer
	}

	assets, err := desiredAgentServiceAssets(s.paths, s.serviceOptions, s.systemdDir, s.recoveryScript, currentBinaryPath)
	if err != nil {
		return agentbinary.ServicePlan{}, err
	}
	for _, asset := range assets {
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
	loadState, err := utilexec.OutputCmdAt(ctx, log, slog.LevelDebug, "systemctl", "show", "--property=LoadState", "--value", service)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(loadState) == "not-found" {
		// Reset removes the unit while intentionally retaining the managed binary
		// layout for a later rejoin.
		return false, nil
	}
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

func (s *HostActivationService) Prepare(_ context.Context, currentBinaryPath string) error {
	return writeAgentServiceAssets(s.paths, s.serviceOptions, s.systemdDir, s.recoveryScript, currentBinaryPath, false)
}

func (s *HostActivationService) Reload(ctx context.Context) error {
	if err := utilexec.ReloadSystemd(ctx, s.log); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	return nil
}

func (s *HostActivationService) Restart(ctx context.Context) error {
	if !s.serviceWasActive {
		s.log.Info("leaving inactive host agent service stopped after activation")
		return nil
	}
	if err := utilexec.RunCmd(ctx, s.log, utilexec.Systemctl(), "restart", ServiceUnitName); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", ServiceUnitName, err)
	}
	return nil
}

// WaitHealthyForBinary preserves an inactive service or waits for the expected
// binary to run stably. The caller synchronizes the worker after this succeeds.
func (s *HostActivationService) WaitHealthyForBinary(ctx context.Context, expectedBinaryPath string) (string, error) {
	expected, err := filepath.EvalSymlinks(expectedBinaryPath)
	if err != nil {
		return "", fmt.Errorf("resolve expected daemon binary: %w", err)
	}
	if !s.serviceWasActive {
		return expected, nil
	}
	var healthySince time.Time
	ticker := time.NewTicker(hostAgentHealthPoll)
	defer ticker.Stop()
	for {
		healthy, checkErr := s.isExpectedDaemonActive(ctx, expected)
		if checkErr == nil && healthy {
			if healthySince.IsZero() {
				healthySince = time.Now()
			} else if time.Since(healthySince) >= hostAgentStableDuration {
				return expected, nil
			}
		} else {
			healthySince = time.Time{}
		}
		select {
		case <-ctx.Done():
			if checkErr != nil {
				return "", fmt.Errorf("daemon did not become healthy: %w", checkErr)
			}
			return "", fmt.Errorf("daemon did not execute expected binary %s: %w", expected, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *HostActivationService) isExpectedDaemonActive(ctx context.Context, expected string) (bool, error) {
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
