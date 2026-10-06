package config

import (
	"maps"
	"strings"
	"testing"
)

func TestMachineNodeLabel(t *testing.T) {
	t.Parallel()
	for name, machine := range map[string]string{
		"ARM name":         "arm-worker",
		"maximum label":    strings.Repeat("a", 63),
		"long DNS name":    strings.Repeat("a", 63) + ".b",
		"maximum DNS name": strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			custom := map[string]string{
				"customer": "preserved", MachineNodeLabel: "wrong",
				managedNodeLabel: "true", agentPoolNodeLabel: "wrong",
				modeNodeLabel: "system", nodePoolTypeNodeLabel: "wrong",
			}
			cfg := &Config{
				Agent: AgentConfig{NodeName: machine},
				Azure: AzureConfig{TargetAgentPoolName: "pool"},
				Node:  NodeConfig{Labels: maps.Clone(custom)},
			}
			for _, slot := range []string{"kube1", "kube2"} {
				agent := ToAgentConfig(cfg, slot)
				want := machine
				if len(machine) > 63 {
					want = ""
				}
				if got := agent.Kubelet.Labels[MachineNodeLabel]; got != want {
					t.Fatalf("slot %s label = %q, want %q", slot, got, want)
				}
				if agent.MachineName != slot {
					t.Fatalf("nspawn slot changed to %q", agent.MachineName)
				}
				for key, value := range map[string]string{
					"customer": "preserved", managedNodeLabel: "false",
					agentPoolNodeLabel: "pool", modeNodeLabel: userNodeMode,
					nodePoolTypeNodeLabel: flexNodePoolType,
				} {
					if agent.Kubelet.Labels[key] != value {
						t.Errorf("label %s = %q, want %q", key, agent.Kubelet.Labels[key], value)
					}
				}
			}
			if !maps.Equal(cfg.Node.Labels, custom) {
				t.Fatal("custom goal labels mutated")
			}
		})
	}
}
