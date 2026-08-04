package daemon

import (
	"context"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestFindActiveMachine(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state   *State
		want    string
		wantErr bool
	}{
		"kube1": {
			state: &State{ActiveMachine: goalstates.NSpawnMachineKube1},
			want:  goalstates.NSpawnMachineKube1,
		},
		"kube2": {
			state: &State{ActiveMachine: goalstates.NSpawnMachineKube2},
			want:  goalstates.NSpawnMachineKube2,
		},
		"missing state": {
			wantErr: true,
		},
		"missing active machine": {
			state:   &State{},
			wantErr: true,
		},
		"invalid active machine": {
			state:   &State{ActiveMachine: "kube3"},
			wantErr: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := (&nspawnNodeOperator{state: &testStateStore{state: tt.state}}).findActiveMachine(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("findActiveMachine error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("findActiveMachine: %v", err)
			}
			if got.Name != tt.want {
				t.Fatalf("machine = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestApplyMachineGoalToConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Components: config.ComponentsConfig{Kubernetes: "1.33.0"},
		Node: config.NodeConfig{
			MaxPods: 110,
			Labels:  map[string]string{"source": "local"},
			Taints:  []string{"local=true:NoSchedule"},
			Kubelet: config.KubeletConfig{
				Verbosity:            4,
				ImageGCHighThreshold: 85,
				ImageGCLowThreshold:  80,
				ImageCredentialProvider: &config.ImageCredentialProviderConfig{
					ConfigPath: "/etc/kubernetes/credential-provider.yaml",
					BinDir:     "/usr/local/lib/kubelet-credential-providers",
				},
			},
		},
	}
	goal := aksmachine.GoalState{
		KubernetesVersion: "1.34.1",
		MaxPods:           42,
		NodeLabels:        map[string]string{"source": "remote"},
		NodeTaints:        []string{"remote=true:NoExecute"},
		KubeletConfig: aksmachine.KubeletConfig{
			ImageGCHighThreshold: 90,
			ImageGCLowThreshold:  75,
		},
	}

	applyMachineGoalToConfig(cfg, goal)

	if cfg.Components.Kubernetes != "1.34.1" || cfg.Node.MaxPods != 42 {
		t.Fatalf("version=%q maxPods=%d, want 1.34.1 and 42", cfg.Components.Kubernetes, cfg.Node.MaxPods)
	}
	if cfg.Node.Labels["source"] != "remote" || cfg.Node.Taints[0] != "remote=true:NoExecute" {
		t.Fatalf("labels=%v taints=%v, want remote settings", cfg.Node.Labels, cfg.Node.Taints)
	}
	if cfg.Node.Kubelet.ImageGCHighThreshold != 90 || cfg.Node.Kubelet.ImageGCLowThreshold != 75 {
		t.Fatalf("kubelet GC thresholds=%d/%d, want 90/75", cfg.Node.Kubelet.ImageGCHighThreshold, cfg.Node.Kubelet.ImageGCLowThreshold)
	}
	if cfg.Node.Kubelet.Verbosity != 4 {
		t.Fatal("local-only kubelet verbosity was not preserved")
	}
	if cfg.Node.Kubelet.ImageCredentialProvider == nil {
		t.Fatal("local image credential provider was not preserved")
	}

	goal.NodeLabels["source"] = "mutated"
	goal.NodeTaints[0] = "mutated=true:NoSchedule"
	if cfg.Node.Labels["source"] != "remote" || cfg.Node.Taints[0] != "remote=true:NoExecute" {
		t.Fatal("applied config shares mutable goal state")
	}
}

func TestApplyMachineGoalToConfigPreservesSettingsOmittedByMachineAPI(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Components: config.ComponentsConfig{Kubernetes: "1.33.0"},
		Node: config.NodeConfig{
			MaxPods: 110,
			Labels:  map[string]string{"source": "local"},
			Taints:  []string{"local=true:NoSchedule"},
			Kubelet: config.KubeletConfig{
				ImageGCHighThreshold: 85,
				ImageGCLowThreshold:  80,
			},
		},
	}

	applyMachineGoalToConfig(cfg, aksmachine.GoalState{KubernetesVersion: "1.34.1"})

	if cfg.Components.Kubernetes != "1.34.1" {
		t.Fatalf("Kubernetes version=%q, want 1.34.1", cfg.Components.Kubernetes)
	}
	if cfg.Node.MaxPods != 110 || cfg.Node.Labels["source"] != "local" || cfg.Node.Taints[0] != "local=true:NoSchedule" {
		t.Fatalf("omitted machine settings replaced local values: %#v", cfg.Node)
	}
	if cfg.Node.Kubelet.ImageGCHighThreshold != 85 || cfg.Node.Kubelet.ImageGCLowThreshold != 80 {
		t.Fatalf("omitted GC thresholds replaced local values: %#v", cfg.Node.Kubelet)
	}
}

type testStateStore struct {
	state *State
}

func (s *testStateStore) Load(context.Context) (*State, error) {
	return s.state, nil
}

func (s *testStateStore) Save(context.Context, *State) error {
	return nil
}

func (s *testStateStore) Delete(context.Context) error {
	return nil
}
