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

	goal, err := GoalStateFromConfig(cfg)
	if err != nil {
		t.Fatalf("GoalStateFromConfig() error = %v", err)
	}
	if goal.KubernetesVersion != "1.35.1" {
		t.Fatalf("KubernetesVersion = %q, want 1.35.1", goal.KubernetesVersion)
	}
	if goal.SettingsVersion != "" {
		t.Fatalf("SettingsVersion = %q, want empty before Machine persistence", goal.SettingsVersion)
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
	_, err := GoalStateFromConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "kubernetes version is empty") {
		t.Fatalf("GoalStateFromConfig() error = %v, want Kubernetes version validation", err)
	}
}

func TestMachineValidate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		machine *Machine
		wantErr string
	}{
		"nil machine": {
			wantErr: "machine is nil",
		},
		"missing Kubernetes version": {
			machine: &Machine{Goal: MachineGoal{SettingsVersion: "42"}},
			wantErr: "kubernetes version is empty",
		},
		"missing settings version": {
			machine: &Machine{Goal: testMachineGoal("1.35.1", "")},
			wantErr: "goal settings version is empty",
		},
		"complete machine": {
			machine: &Machine{Goal: testMachineGoal("1.35.1", "42")},
		},
		"omitted scalar defaults": {
			machine: &Machine{Goal: MachineGoal{KubernetesVersion: "1.35.1", SettingsVersion: "42"}},
		},
		"invalid present max pods": {
			machine: &Machine{Goal: MachineGoal{KubernetesVersion: "1.35.1", SettingsVersion: "42", MaxPods: ptr(-1)}},
			wantErr: "max pods must be positive",
		},
		"invalid present image GC thresholds": {
			machine: &Machine{Goal: MachineGoal{
				KubernetesVersion: "1.35.1",
				SettingsVersion:   "42",
				KubeletConfig: MachineKubeletConfig{
					ImageGCHighThreshold: ptr(70),
					ImageGCLowThreshold:  ptr(80),
				},
			}},
			wantErr: "image GC low threshold must be less than image GC high threshold",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tt.machine.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestEffectiveGoal(t *testing.T) {
	t.Parallel()

	local := testGoal("1.34.0", "")
	local.MaxPods = 30
	local.NodeLabels = map[string]string{"source": "local"}
	local.NodeTaints = []string{"local=true:NoSchedule"}
	local.KubeletConfig.ImageGCHighThreshold = 90
	local.KubeletConfig.ImageGCLowThreshold = 75
	machine := MachineGoal{
		KubernetesVersion: "1.35.0",
		SettingsVersion:   "42",
		NodeLabels:        map[string]string{},
		NodeTaints:        []string{},
	}

	effective, err := EffectiveGoal(machine, local)
	if err != nil {
		t.Fatalf("EffectiveGoal() error = %v", err)
	}
	if effective.KubernetesVersion != "1.35.0" || effective.SettingsVersion != "42" || effective.MaxPods != 30 {
		t.Fatalf("effective versions/maxPods = %#v", effective)
	}
	if len(effective.NodeLabels) != 0 || len(effective.NodeTaints) != 0 {
		t.Fatalf("effective collections = %#v, want authoritative empty collections", effective)
	}
	if effective.KubeletConfig.ImageGCHighThreshold != 90 || effective.KubeletConfig.ImageGCLowThreshold != 75 {
		t.Fatalf("effective kubelet config = %#v", effective.KubeletConfig)
	}

	effective.NodeLabels["source"] = "changed"
	if _, ok := machine.NodeLabels["source"]; ok {
		t.Fatal("EffectiveGoal returned Machine-owned label map")
	}
}

func TestEffectiveGoalPreservesExplicitZero(t *testing.T) {
	t.Parallel()

	local := testGoal("1.34.0", "")
	machine := MachineGoal{
		KubernetesVersion: "1.35.0",
		SettingsVersion:   "42",
		KubeletConfig: MachineKubeletConfig{
			ImageGCLowThreshold: ptr(0),
		},
	}

	effective, err := EffectiveGoal(machine, local)
	if err != nil {
		t.Fatalf("EffectiveGoal() error = %v", err)
	}
	if effective.KubeletConfig.ImageGCLowThreshold != 0 {
		t.Fatalf("ImageGCLowThreshold = %d, want explicit zero", effective.KubeletConfig.ImageGCLowThreshold)
	}
}
