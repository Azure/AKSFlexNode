package arc

import (
	"context"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const arcConnectedMachineCheckName = "arc-connected-machine"

type connectedMachineCheckDeps struct {
	lookupPath      func(string) (string, error)
	isServiceActive func(context.Context, *slog.Logger, string) bool
	output          func(context.Context, *slog.Logger, string, ...string) (string, error)
}

type connectedMachineChecker struct {
	log  *slog.Logger
	deps connectedMachineCheckDeps
}

// Preflight returns checks for an externally managed Azure Arc connection.
func Preflight(cfg *config.Config, log *slog.Logger) []preflight.Checker {
	if !cfg.IsARCEnabled() {
		return nil
	}
	return []preflight.Checker{connectedMachineChecker{
		log: log,
		deps: connectedMachineCheckDeps{
			lookupPath:      exec.LookPath,
			isServiceActive: utilexec.IsServiceActive,
			output:          utilexec.OutputCmd,
		},
	}}
}

func (connectedMachineChecker) Name() string { return arcConnectedMachineCheckName }

func (c connectedMachineChecker) Check(ctx context.Context) []preflight.Result {
	const target = "Azure Connected Machine agent"
	if _, err := c.deps.lookupPath("azcmagent"); err != nil {
		return preflight.ResultsError(arcConnectedMachineCheckName, target, "azcmagent is not installed")
	}
	if !c.deps.isServiceActive(ctx, c.log, "himdsd") {
		return preflight.ResultsError(arcConnectedMachineCheckName, target, "Arc identity service himdsd is not active")
	}
	output, err := c.deps.output(ctx, c.log, "azcmagent", "show")
	if err != nil {
		return preflight.ResultsError(arcConnectedMachineCheckName, target, "failed to inspect Arc connection status")
	}
	if !arcAgentConnected(output) {
		return preflight.ResultsError(arcConnectedMachineCheckName, target, "Azure Connected Machine agent is not connected")
	}
	return preflight.ResultsOK(arcConnectedMachineCheckName, target, "Azure Connected Machine agent and identity service are ready")
}

func arcAgentConnected(output string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Agent Status") {
			return strings.EqualFold(strings.TrimSpace(value), "connected")
		}
	}
	return false
}

var _ preflight.Checker = connectedMachineChecker{}
