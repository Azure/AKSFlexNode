package arc

import (
	"context"
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"

	"github.com/Azure/AKSFlexNode/pkg/utils"
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

func isArcServicesRunning(ctx context.Context) bool {
	if !isArcAgentInstalled() {
		return false
	}
	for _, service := range arcServices {
		if !utils.IsServiceActive(service) {
			return false
		}
	}
	cmd := exec.CommandContext(ctx, "pgrep", "-f", "azcmagent")
	return cmd.Run() == nil
}

func ptrDeref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
