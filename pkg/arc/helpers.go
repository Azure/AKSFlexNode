package arc

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
)

func isArcAgentInstalled() bool {
	_, err := exec.LookPath("azcmagent")
	return err == nil
}

func getArcMachineIdentityID(arcMachine *armhybridcompute.Machine) string {
	if arcMachine != nil &&
		arcMachine.Identity != nil &&
		arcMachine.Identity.PrincipalID != nil {
		return *arcMachine.Identity.PrincipalID
	}
	return ""
}

func isArcServicesRunning(ctx context.Context, logger *slog.Logger) bool {
	if !isArcAgentInstalled() {
		return false
	}
	for _, service := range arcServices {
		if !utilexec.IsServiceActive(ctx, logger, service) {
			return false
		}
	}
	return utilexec.RunCmdAt(ctx, logger, slog.LevelDebug, utilexec.Pgrep(), "-f", "azcmagent") == nil
}

func ptrDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
