package daemon

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

type machinesTargetChecker struct {
	log    *slog.Logger
	output func(context.Context, *slog.Logger, string, ...string) (string, error)
}

// CheckMachinesTarget checks that systemd can start nspawn workers at boot.
func CheckMachinesTarget(log *slog.Logger) preflight.Checker {
	return machinesTargetChecker{log: log, output: utilexec.OutputCmd}
}

func (machinesTargetChecker) Name() string { return "machines-target" }

func (c machinesTargetChecker) Check(ctx context.Context) []preflight.Result {
	const target = "machines.target"
	state, err := c.output(ctx, c.log, "systemctl", "show", "--property=UnitFileState", "--value", target)
	if err != nil {
		return preflight.ResultsError(c.Name(), target, "failed to inspect machines.target")
	}
	switch strings.TrimSpace(state) {
	case "enabled", "static":
		return preflight.ResultsOK(c.Name(), target, "machines.target is available for worker startup")
	case "disabled", "enabled-runtime":
		return preflight.ResultsWarning(c.Name(), target, "machines.target is not enabled persistently; bootstrap installs a daemon Wants=machines.target dependency to start workers at boot")
	default:
		return preflight.ResultsError(c.Name(), target, "machines.target is missing, masked, or unavailable; install systemd-container and unmask machines.target before bootstrap")
	}
}
