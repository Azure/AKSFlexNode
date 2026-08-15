package daemon

import (
	"log/slog"
	"maps"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestResolveMachineGoalStateUsesCompleteMachineGoal(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Azure:      config.AzureConfig{TargetAgentPoolName: "flexnode-edge"},
		Components: config.ComponentsConfig{Kubernetes: "1.34.0"},
		Node: config.NodeConfig{
			MaxPods: 30,
			Labels:  map[string]string{"source": "config"},
			Taints:  []string{"config=true:NoSchedule"},
			Kubelet: config.KubeletConfig{ImageGCHighThreshold: 90, ImageGCLowThreshold: 75},
		},
	}
	goal := testMachineGoal("1.35.1", "42")
	goal.MaxPods = 50
	goal.NodeLabels = map[string]string{"source": "machine"}
	goal.NodeTaints = []string{"machine=true:NoExecute"}
	goal.KubeletConfig.ImageGCHighThreshold = 70
	goal.KubeletConfig.ImageGCLowThreshold = 60

	agentCfg, _, _, err := ResolveMachineGoalState(t.Context(), slog.Default(), cfg, "kube1", &goal)
	if err != nil {
		t.Fatalf("ResolveMachineGoalState: %v", err)
	}
	if agentCfg.Cluster.Version != "1.35.1" {
		t.Fatalf("Cluster.Version = %q, want 1.35.1", agentCfg.Cluster.Version)
	}
	if got := agentCfg.Kubelet.Configuration["maxPods"]; got != 50 {
		t.Fatalf("maxPods = %v, want 50", got)
	}
	if got := agentCfg.Kubelet.Configuration["imageGCHighThresholdPercent"]; got != 70 {
		t.Fatalf("imageGCHighThresholdPercent = %v, want 70", got)
	}
	if got := agentCfg.Kubelet.Configuration["imageGCLowThresholdPercent"]; got != 60 {
		t.Fatalf("imageGCLowThresholdPercent = %v, want 60", got)
	}
	if agentCfg.Kubelet.Labels["source"] != "machine" {
		t.Fatalf("Kubelet.Labels = %#v, want Machine labels", agentCfg.Kubelet.Labels)
	}
	if len(agentCfg.Kubelet.RegisterWithTaints) != 1 || agentCfg.Kubelet.RegisterWithTaints[0] != "machine=true:NoExecute" {
		t.Fatalf("RegisterWithTaints = %#v", agentCfg.Kubelet.RegisterWithTaints)
	}
	if cfg.Components.Kubernetes != "1.34.0" || cfg.Node.MaxPods != 30 || cfg.Node.Labels["source"] != "config" {
		t.Fatalf("base config was mutated: %#v", cfg)
	}
}

func TestGoalForRestartLegacyStatePreservesConfigSettings(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Components: config.ComponentsConfig{Kubernetes: "1.34.0"},
		Node: config.NodeConfig{
			MaxPods: 30,
			Labels:  map[string]string{"source": "config"},
			Taints:  []string{"config=true:NoSchedule"},
			Kubelet: config.KubeletConfig{ImageGCHighThreshold: 90, ImageGCLowThreshold: 75},
		},
	}
	state := &State{AppliedSettingsVersion: "42", AppliedKubernetesVersion: "1.35.1"}

	goal, err := goalForRestart(cfg, state)
	if err != nil {
		t.Fatalf("goalForRestart: %v", err)
	}
	if goal.KubernetesVersion != "1.35.1" || goal.SettingsVersion != "42" || goal.MaxPods != 30 {
		t.Fatalf("goal versions/maxPods = %#v", goal)
	}
	if !maps.Equal(goal.NodeLabels, cfg.Node.Labels) || len(goal.NodeTaints) != 1 || goal.NodeTaints[0] != cfg.Node.Taints[0] {
		t.Fatalf("legacy restart goal lost config settings: %#v", goal)
	}
	if goal.KubeletConfig.ImageGCHighThreshold != 90 || goal.KubeletConfig.ImageGCLowThreshold != 75 {
		t.Fatalf("legacy restart kubelet config = %#v", goal.KubeletConfig)
	}
}

func TestGoalForRestartClonesCompleteGoal(t *testing.T) {
	t.Parallel()

	applied := testMachineGoal("1.35.1", "42")
	applied.NodeLabels = map[string]string{"source": "machine"}
	state := &State{AppliedGoal: &applied}

	goal, err := goalForRestart(&config.Config{}, state)
	if err != nil {
		t.Fatalf("goalForRestart: %v", err)
	}
	goal.NodeLabels["source"] = "changed"
	if state.AppliedGoal.NodeLabels["source"] != "machine" {
		t.Fatal("goalForRestart returned state-owned label map")
	}
}
