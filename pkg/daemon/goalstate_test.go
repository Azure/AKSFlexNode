package daemon

import (
	"log/slog"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestResolveMachineGoalStateUsesMachineGoal(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Azure:      config.AzureConfig{TargetAgentPoolName: "flexnode-edge"},
		Components: config.ComponentsConfig{Kubernetes: "1.34.0"},
		Node: config.NodeConfig{
			Labels: map[string]string{"source": "bootstrap"},
			Taints: []string{"bootstrap=true:NoSchedule"},
		},
	}
	goal := &aksmachine.GoalState{
		KubernetesVersion: "1.35.1",
		NodeLabels:        map[string]string{"source": "machine"},
		NodeTaints:        []string{"machine=true:NoExecute"},
	}

	agentCfg, _, _, err := ResolveMachineGoalState(t.Context(), slog.Default(), cfg, "kube1", goal)
	if err != nil {
		t.Fatalf("ResolveMachineGoalState: %v", err)
	}
	if agentCfg.Cluster.Version != "1.35.1" {
		t.Fatalf("Cluster.Version = %q, want 1.35.1", agentCfg.Cluster.Version)
	}
	if agentCfg.Kubelet.Labels["source"] != "machine" {
		t.Fatalf("Kubelet.Labels = %#v, want Machine goal labels", agentCfg.Kubelet.Labels)
	}
	if len(agentCfg.Kubelet.RegisterWithTaints) != 1 || agentCfg.Kubelet.RegisterWithTaints[0] != "machine=true:NoExecute" {
		t.Fatalf("Kubelet.RegisterWithTaints = %#v, want Machine goal taints", agentCfg.Kubelet.RegisterWithTaints)
	}
	if cfg.Components.Kubernetes != "1.34.0" || cfg.Node.Labels["source"] != "bootstrap" || cfg.Node.Taints[0] != "bootstrap=true:NoSchedule" {
		t.Fatalf("bootstrap config was mutated: %#v", cfg)
	}
}
