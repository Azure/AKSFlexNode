package aksmachine

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

const testClusterResourceID = "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster"

func TestMachineResourceIDFromConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     *config.Config
		want    string
		wantErr string
	}{
		{
			name: "valid config",
			cfg: testARMConfig(
				testClusterResourceID,
				"flex-node-1",
				"1.34.0",
			),
			want: testClusterResourceID + "/agentPools/aksflexnodes/machines/flex-node-1",
		},
		{
			name: "trims cluster resource slash",
			cfg: testARMConfig(
				testClusterResourceID+"/",
				"flex-node-1",
				"1.34.0",
			),
			want: testClusterResourceID + "/agentPools/aksflexnodes/machines/flex-node-1",
		},
		{
			name: "missing cluster resource ID",
			cfg: testARMConfig(
				"",
				"flex-node-1",
				"1.34.0",
			),
			wantErr: "incomplete AKS machine config",
		},
		{
			name: "missing node name",
			cfg: testARMConfig(
				testClusterResourceID,
				"",
				"1.34.0",
			),
			wantErr: "incomplete AKS machine config",
		},
		{
			name: "missing Kubernetes version",
			cfg: testARMConfig(
				testClusterResourceID,
				"flex-node-1",
				"",
			),
			wantErr: "incomplete AKS machine config",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := machineResourceIDFromConfig(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("machineResourceIDFromConfig() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("machineResourceIDFromConfig() error = %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("machineResourceIDFromConfig() = %q, want %q", got.String(), tt.want)
			}
			if got.Parent == nil || got.Parent.Name != aksFlexNodePoolName {
				t.Fatalf("agent pool parent = %#v, want name %q", got.Parent, aksFlexNodePoolName)
			}
			if got.Parent.Parent == nil || got.Parent.Parent.Name != "test-cluster" {
				t.Fatalf("cluster parent = %#v, want name test-cluster", got.Parent.Parent)
			}
		})
	}
}

func TestBuildK8sProfile(t *testing.T) {
	t.Parallel()

	profile := buildK8sProfile(GoalState{
		KubernetesVersion: "1.35.1",
		MaxPods:           42,
		NodeLabels:        map[string]string{"workload": "flex"},
		NodeTaints:        []string{"dedicated=flex:NoSchedule"},
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	})
	if profile.OrchestratorVersion == nil || *profile.OrchestratorVersion != "1.35.1" {
		t.Fatalf("OrchestratorVersion = %v, want 1.35.1", profile.OrchestratorVersion)
	}
	if profile.MaxPods == nil || *profile.MaxPods != 42 {
		t.Fatalf("MaxPods = %v, want 42", profile.MaxPods)
	}
	if got := profile.NodeLabels["workload"]; got == nil || *got != "flex" {
		t.Fatalf("NodeLabels[workload] = %v, want flex", got)
	}
	if len(profile.NodeTaints) != 1 || profile.NodeTaints[0] == nil || *profile.NodeTaints[0] != "dedicated=flex:NoSchedule" {
		t.Fatalf("NodeTaints = %#v, want dedicated=flex:NoSchedule", profile.NodeTaints)
	}
	if profile.KubeletConfig == nil {
		t.Fatal("KubeletConfig is nil")
	}
	if profile.KubeletConfig.ImageGcHighThreshold == nil || *profile.KubeletConfig.ImageGcHighThreshold != 85 {
		t.Fatalf("ImageGcHighThreshold = %v, want 85", profile.KubeletConfig.ImageGcHighThreshold)
	}
	if profile.KubeletConfig.ImageGcLowThreshold == nil || *profile.KubeletConfig.ImageGcLowThreshold != 80 {
		t.Fatalf("ImageGcLowThreshold = %v, want 80", profile.KubeletConfig.ImageGcLowThreshold)
	}
}

func TestGoalStateValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		goal    GoalState
		wantErr string
	}{
		{
			name: "valid",
			goal: GoalState{KubernetesVersion: "1.35.1"},
		},
		{
			name:    "missing Kubernetes version",
			goal:    GoalState{},
			wantErr: "kubernetes version is empty",
		},
		{
			name:    "negative max pods",
			goal:    GoalState{KubernetesVersion: "1.35.1", MaxPods: -1},
			wantErr: "max pods must be non-negative",
		},
		{
			name: "negative image GC high threshold",
			goal: GoalState{
				KubernetesVersion: "1.35.1",
				KubeletConfig: KubeletConfig{
					ImageGCHighThreshold: -1,
				},
			},
			wantErr: "image GC high threshold must be non-negative",
		},
		{
			name: "negative image GC low threshold",
			goal: GoalState{
				KubernetesVersion: "1.35.1",
				KubeletConfig: KubeletConfig{
					ImageGCLowThreshold: -1,
				},
			},
			wantErr: "image GC low threshold must be non-negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.goal.validate()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validate() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() error = %v", err)
			}
		})
	}
}

func TestMachineFromARM(t *testing.T) {
	t.Parallel()

	orchestratorVersion := "1.35.1"
	provisioningState := "Succeeded"
	machine := machineFromARM(armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				OrchestratorVersion: &orchestratorVersion,
			},
			ProvisioningState: &provisioningState,
		},
	}, GoalState{SettingsVersion: "fallback-settings"})

	if machine.ID != "" {
		t.Fatalf("ID = %q, want empty", machine.ID)
	}
	if machine.Name != "" {
		t.Fatalf("Name = %q, want empty", machine.Name)
	}
	if machine.Goal.KubernetesVersion != orchestratorVersion {
		t.Fatalf("KubernetesVersion = %q, want %q", machine.Goal.KubernetesVersion, orchestratorVersion)
	}
	if machine.Goal.SettingsVersion != "fallback-settings" {
		t.Fatalf("SettingsVersion = %q, want fallback-settings", machine.Goal.SettingsVersion)
	}
	if machine.Status.ProvisioningState != ProvisioningStateSucceeded {
		t.Fatalf("ProvisioningState = %q, want %q", machine.Status.ProvisioningState, ProvisioningStateSucceeded)
	}
}

func TestValidateMachineIdentity(t *testing.T) {
	t.Parallel()

	machineID, err := machineResourceIDFromConfig(testARMConfig(testClusterResourceID, "flex-node-1", "1.34.0"))
	if err != nil {
		t.Fatalf("machineResourceIDFromConfig() error = %v", err)
	}
	client := &armMachineClient{machineID: machineID}

	tests := []struct {
		name    string
		machine armcontainerservice.Machine
		wantErr string
	}{
		{
			name: "matching identity",
			machine: armcontainerservice.Machine{
				ID:   ptr(machineID.String()),
				Name: ptr("flex-node-1"),
			},
		},
		{
			name:    "missing remote identity is allowed",
			machine: armcontainerservice.Machine{},
		},
		{
			name: "ID mismatch",
			machine: armcontainerservice.Machine{
				ID: ptr(testClusterResourceID + "/agentPools/aksflexnodes/machines/other-node"),
			},
			wantErr: "AKS machine ID mismatch",
		},
		{
			name: "name mismatch",
			machine: armcontainerservice.Machine{
				Name: ptr("other-node"),
			},
			wantErr: "AKS machine name mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := client.validateMachineIdentity(tt.machine)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("validateMachineIdentity() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateMachineIdentity() error = %v", err)
			}
		})
	}
}

func TestMachineFromARMUsesCurrentOrchestratorVersionFallback(t *testing.T) {
	t.Parallel()

	currentVersion := "1.35.2"
	machine := machineFromARM(armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				CurrentOrchestratorVersion: &currentVersion,
			},
		},
	}, GoalState{})

	if machine.Goal.KubernetesVersion != currentVersion {
		t.Fatalf("KubernetesVersion = %q, want %q", machine.Goal.KubernetesVersion, currentVersion)
	}
	if machine.Goal.SettingsVersion != currentVersion {
		t.Fatalf("SettingsVersion = %q, want %q", machine.Goal.SettingsVersion, currentVersion)
	}
}

func testARMConfig(clusterResourceID, nodeName, kubernetesVersion string) *config.Config {
	return &config.Config{
		Azure: config.AzureConfig{
			TargetCluster: &config.TargetClusterConfig{
				ResourceID: clusterResourceID,
			},
		},
		Agent: config.AgentConfig{
			NodeName: nodeName,
		},
		Kubernetes: config.KubernetesConfig{
			Version: kubernetesVersion,
		},
	}
}

func ptr[T any](v T) *T {
	return &v
}
