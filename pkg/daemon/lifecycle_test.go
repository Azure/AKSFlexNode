package daemon

import (
	"strings"
	"testing"
)

func TestAgentServiceIncludesUpgradeRecovery(t *testing.T) {
	t.Parallel()

	service := string(serviceUnitContent)
	if !strings.Contains(service, "OnFailure="+recoveryServiceUnitName) {
		t.Fatalf("service does not activate %s on failure", recoveryServiceUnitName)
	}
	if !strings.Contains(string(recoveryServiceUnitContent), "ExecStart="+recoveryScriptPath) {
		t.Fatalf("recovery service does not execute %s", recoveryScriptPath)
	}
	script := string(recoveryScriptContent)
	for _, expected := range []string{
		"recover-agent-upgrade",
		"aks-flex-node-agent.service",
		"aks-flex-node-last-good",
		"agent-upgrade-signal.json",
		"systemctl --no-block restart",
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("recovery script does not contain %q", expected)
		}
	}
}
