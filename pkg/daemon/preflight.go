package daemon

import (
	"context"
	"log/slog"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

type machinesTargetChecker struct {
	log            *slog.Logger
	failForMissing bool
	output         func(context.Context, *slog.Logger, string, ...string) (string, error)
}

// CheckMachinesTarget checks that systemd can start nspawn workers at boot.
// TODO: Move this check to Unbounded's host preflight checks.
func CheckMachinesTarget(log *slog.Logger, failForMissing bool) preflight.Checker {
	return machinesTargetChecker{log: log, failForMissing: failForMissing, output: utilexec.OutputCmd}
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
	case "", "not-found":
		if c.failForMissing {
			return preflight.ResultsError(c.Name(), target, "machines.target is missing; install systemd-container on the host before offline or containerized bootstrap")
		}
		return preflight.ResultsWarning(c.Name(), target, "machines.target is missing and may be installed by online bootstrap with systemd-container")
	case "masked", "masked-runtime":
		return preflight.ResultsError(c.Name(), target, "machines.target is masked; unmask it before bootstrap")
	default:
		return preflight.ResultsError(c.Name(), target, "machines.target has unexpected UnitFileState %q", strings.TrimSpace(state))
	}
}
