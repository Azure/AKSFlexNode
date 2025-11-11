package arc

import (
	"os/exec"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/hybridcompute/armhybridcompute"
	"go.goms.io/aks/AKSFlexNode/pkg/utils"
)

func isArcAgentInstalled() bool {
	_, err := exec.LookPath("azcmagent")
	return err == nil
}

func isArcServicesRunning() bool {
	if !isArcAgentInstalled() {
		return false
	}

	for _, service := range arcServices {
		if !utils.IsServiceActive(service) {
			return false
		}
	}

	cmd := exec.Command("pgrep", "-f", "azcmagent")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

func getArcMachineIdentityID(arcMachine *armhybridcompute.Machine) string {
	if arcMachine != nil &&
		arcMachine.Identity != nil &&
		arcMachine.Identity.PrincipalID != nil {
		return *arcMachine.Identity.PrincipalID
	}
	return ""
}
