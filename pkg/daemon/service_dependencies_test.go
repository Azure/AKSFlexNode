package daemon

import (
	"slices"
	"strings"
	"testing"
)

func TestDaemonUnitsWantMachinesTarget(t *testing.T) {
	t.Parallel()

	hostUnit, err := renderAgentServiceUnit("/usr/local/bin/aks-flex-node", agentServiceOptions{WantedTargets: (nspawnProvider{}).WantedTargets()})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"host":      hostUnit,
		"container": containerServiceUnitContent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, line := range strings.Split(string(content), "\n") {
				if wants, ok := strings.CutPrefix(line, "Wants="); ok && slices.Contains(strings.Fields(wants), "machines.target") {
					return
				}
			}
			t.Fatal("daemon unit must want machines.target so workers start even when the target is disabled")
		})
	}
}
