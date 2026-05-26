package aksmachine

import (
	"context"
	"strings"
	"testing"
)

type fakeMachineClient struct{}

func (fakeMachineClient) Create(context.Context, GoalState) (*Machine, error) { return nil, nil }
func (fakeMachineClient) Get(context.Context) (*Machine, error)               { return nil, nil }
func (fakeMachineClient) PatchStatus(context.Context, Status) error           { return nil }

func TestFakeMachineClientImplementsInterface(t *testing.T) {
	t.Parallel()

	var _ MachineClient = fakeMachineClient{}
}

func TestGoalStateFromConfig(t *testing.T) {
	t.Parallel()

	cfg := testARMConfig(testClusterResourceID, "flex-node-1", "1.35.1")
	cfg.Node.MaxPods = 42
	cfg.Node.Labels = map[string]string{
		"workload": "flex",
		"zone":     "edge",
	}
	cfg.Node.Taints = []string{
		"dedicated=flex:NoSchedule",
		"edge=true:NoExecute",
	}
	cfg.Node.Kubelet.ImageGCHighThreshold = 85
	cfg.Node.Kubelet.ImageGCLowThreshold = 80

	goal, err := goalStateFromConfig(cfg)
	if err != nil {
		t.Fatalf("goalStateFromConfig() error = %v", err)
	}
	if goal.KubernetesVersion != "1.35.1" {
		t.Fatalf("KubernetesVersion = %q, want 1.35.1", goal.KubernetesVersion)
	}
	if goal.MaxPods != 42 {
		t.Fatalf("MaxPods = %d, want 42", goal.MaxPods)
	}
	if len(goal.NodeLabels) != 2 {
		t.Fatalf("NodeLabels length = %d, want 2", len(goal.NodeLabels))
	}
	if got := goal.NodeLabels["workload"]; got != "flex" {
		t.Fatalf("NodeLabels[workload] = %v, want flex", got)
	}
	if got := goal.NodeLabels["zone"]; got != "edge" {
		t.Fatalf("NodeLabels[zone] = %v, want edge", got)
	}
	if len(goal.NodeTaints) != 2 {
		t.Fatalf("NodeTaints length = %d, want 2", len(goal.NodeTaints))
	}
	if goal.NodeTaints[0] != "dedicated=flex:NoSchedule" {
		t.Fatalf("NodeTaints[0] = %v, want dedicated=flex:NoSchedule", goal.NodeTaints[0])
	}
	if goal.NodeTaints[1] != "edge=true:NoExecute" {
		t.Fatalf("NodeTaints[1] = %v, want edge=true:NoExecute", goal.NodeTaints[1])
	}
	if goal.KubeletConfig.ImageGCHighThreshold != 85 {
		t.Fatalf("ImageGCHighThreshold = %d, want 85", goal.KubeletConfig.ImageGCHighThreshold)
	}
	if goal.KubeletConfig.ImageGCLowThreshold != 80 {
		t.Fatalf("ImageGCLowThreshold = %d, want 80", goal.KubeletConfig.ImageGCLowThreshold)
	}
}

func TestGoalStateFromConfigValidates(t *testing.T) {
	t.Parallel()

	cfg := testARMConfig(testClusterResourceID, "flex-node-1", "")
	_, err := goalStateFromConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "kubernetes version is empty") {
		t.Fatalf("goalStateFromConfig() error = %v, want Kubernetes version validation", err)
	}
}
