package daemon

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// ContainerizedEnv marks the agent as running in its container image, which sets it. There, the
// image carries the agent and the host's provisioning its OS packages, so the agent installs
// neither on the host, and the agent's unit runs the image rather than a binary.
const ContainerizedEnv = "AKS_FLEX_NODE_CONTAINERIZED"

// containerServiceUnitContent is the agent's unit in its container: it runs the daemon in a
// container of the agent's image, which the host's provisioning names, or an AgentUpgrade
// selected.
//
//go:embed assets/aks-flex-node-agent-container.service
var containerServiceUnitContent []byte

// containerRecoveryUnitContent is the agent's recovery unit in its container, which restores the
// last-good image when an upgraded agent's unit keeps failing.
//
//go:embed assets/aks-flex-node-agent-recovery-container.service
var containerRecoveryUnitContent []byte

// Containerized reports whether the agent runs in its container image.
func Containerized() bool {
	return containerized(os.Getenv)
}

func containerized(getenv func(string) string) bool {
	on, err := strconv.ParseBool(getenv(ContainerizedEnv))
	return err == nil && on
}

// CredentialPlugin is the exec credential plugin the agent's own Kubernetes clients run with a
// managed identity or a service principal and no bootstrap token, before the daemon has its
// certificate: on a host, the host's agent, config.HostBinaryPath. In the container the host has
// no agent, so they run the image's binary, which is running.
func CredentialPlugin() (string, error) {
	return credentialPlugin(Containerized(), runningAgentExecutable, config.HostBinaryPath)
}

func credentialPlugin(containerized bool, runningExecutable func() (string, error), hostBinary func() string) (string, error) {
	if !containerized {
		return hostBinary(), nil
	}
	return runningExecutable()
}

// containerSkippedPreflightChecks are host checks that don't apply to the agent in its
// container. host-packages checks the image's package database, not the host's, whose packages
// its provisioning provides. containerd-active warns that bootstrap will disable containerd,
// which Flex never does; there, containerd runs the agent.
var containerSkippedPreflightChecks = []string{"host-packages", "containerd-active"}

// PreflightChecks leaves out, in the agent's container, the checks that don't apply there.
func PreflightChecks(checks []preflight.Checker) []preflight.Checker {
	return preflightChecks(checks, Containerized())
}

func preflightChecks(checks []preflight.Checker, containerized bool) []preflight.Checker {
	if !containerized {
		return checks
	}
	return slices.DeleteFunc(slices.Clone(checks), func(check preflight.Checker) bool {
		return slices.Contains(containerSkippedPreflightChecks, check.Name())
	})
}

// ensureHostInstall converges the agent's binary layout and units on the host, as
// ensureDaemonServiceAssets does, and returns what writes those it held back while an
// AgentUpgrade was pending, for the daemon to run once the upgrade is judged. In its container
// the agent has no binary layout, the image carries it, and containerAgentConvergence converges
// its units, so it does nothing there.
func ensureHostInstall(
	ctx context.Context,
	log *slog.Logger,
	cfg *config.Config,
	containerized bool,
) (func(context.Context) error, error) {
	if containerized {
		return noHeldAssets, nil
	}
	held, err := ensureDaemonServiceAssets(ctx, log, cfg)
	if err != nil || !held {
		return noHeldAssets, err
	}
	return func(ctx context.Context) error {
		_, err := ensureDaemonServiceAssets(ctx, log, cfg)
		return err
	}, nil
}

// noHeldAssets is what the daemon runs once an upgrade is judged when it held nothing back.
func noHeldAssets(context.Context) error { return nil }

// installContainerServiceTask installs, enables, and starts the agent's unit in its container,
// with its recovery unit, as installServiceTask does on a host. They have the same names, so
// UninstallService removes them in either case.
type installContainerServiceTask struct {
	log       *slog.Logger
	unitDir   string
	systemctl func(ctx context.Context, args ...string) error
}

func newInstallContainerServiceTask(log *slog.Logger) *installContainerServiceTask {
	return &installContainerServiceTask{
		log:     log,
		unitDir: systemdSystemDir,
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
	}
}

func (t *installContainerServiceTask) Name() string { return "install-service" }

func (t *installContainerServiceTask) Do(ctx context.Context) error {
	if _, err := writeContainerUnits(t.unitDir, false); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ServiceUnitName},
		{"start", ServiceUnitName},
	} {
		if err := t.systemctl(ctx, args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", args[0], err)
		}
	}
	t.log.Info("systemd service installed and started", "unit", ServiceUnitName)
	return nil
}
