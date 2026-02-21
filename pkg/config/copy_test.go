package config

import "testing"

func TestCloneStringMap(t *testing.T) {
	t.Parallel()

	if got := cloneStringMap(nil); got != nil {
		t.Fatalf("cloneStringMap(nil)=%v, want nil", got)
	}

	in := map[string]string{"a": "1"}
	out := cloneStringMap(in)
	if out["a"] != "1" {
		t.Fatalf("cloneStringMap value=%q, want %q", out["a"], "1")
	}

	// Mutate input; output should not change.
	in["a"] = "2"
	if out["a"] != "1" {
		t.Fatalf("cloneStringMap shares backing map; out[a]=%q, want %q", out["a"], "1")
	}

	// Mutate output; input should not change.
	out["a"] = "3"
	if in["a"] != "2" {
		t.Fatalf("cloneStringMap shares backing map; in[a]=%q, want %q", in["a"], "2")
	}
}

func TestConfigDeepCopy_Nil(t *testing.T) {
	t.Parallel()

	var cfg *Config
	if got := cfg.DeepCopy(); got != nil {
		t.Fatalf("DeepCopy()=%v, want nil", got)
	}
}

func TestConfigDeepCopy_DoesNotSharePointersOrMaps(t *testing.T) {
	t.Parallel()

	falseVal := false
	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{TenantID: "t", ClientID: "c", ClientSecret: "s"},
			ManagedIdentity:  &ManagedIdentityConfig{ClientID: "mi"},
			BootstrapToken:   &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
			TargetCluster:    &TargetClusterConfig{ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster", Location: "eastus"},
			Arc:              &ArcConfig{Enabled: true, MachineName: "m", Location: "eastus", ResourceGroup: "rg", Tags: map[string]string{"k": "v"}},
		},
		Agent: AgentConfig{EnableDriftDetectionAndRemediation: &falseVal},
		Node: NodeConfig{
			Labels: map[string]string{"l": "1"},
			Kubelet: KubeletConfig{
				KubeReserved: map[string]string{"cpu": "100m"},
				EvictionHard: map[string]string{"memory.available": "200Mi"},
			},
		},
	}

	copy := cfg.DeepCopy()
	if copy == nil {
		t.Fatalf("DeepCopy()=nil")
	}
	if copy == cfg {
		t.Fatalf("DeepCopy returned same pointer")
	}

	// Pointer sub-objects should not be shared.
	if cfg.Azure.ServicePrincipal == nil || copy.Azure.ServicePrincipal == nil || cfg.Azure.ServicePrincipal == copy.Azure.ServicePrincipal {
		t.Fatalf("ServicePrincipal pointer shared or nil")
	}
	if cfg.Azure.ManagedIdentity == nil || copy.Azure.ManagedIdentity == nil || cfg.Azure.ManagedIdentity == copy.Azure.ManagedIdentity {
		t.Fatalf("ManagedIdentity pointer shared or nil")
	}
	if cfg.Azure.BootstrapToken == nil || copy.Azure.BootstrapToken == nil || cfg.Azure.BootstrapToken == copy.Azure.BootstrapToken {
		t.Fatalf("BootstrapToken pointer shared or nil")
	}
	if cfg.Azure.TargetCluster == nil || copy.Azure.TargetCluster == nil || cfg.Azure.TargetCluster == copy.Azure.TargetCluster {
		t.Fatalf("TargetCluster pointer shared or nil")
	}
	if cfg.Azure.Arc == nil || copy.Azure.Arc == nil || cfg.Azure.Arc == copy.Azure.Arc {
		t.Fatalf("Arc pointer shared or nil")
	}

	// Maps should not be shared (validate via independent mutation behavior).
	cfg.Azure.Arc.Tags["k"] = "orig"
	if copy.Azure.Arc.Tags["k"] != "v" {
		t.Fatalf("Arc.Tags shared; copy=%q, want %q", copy.Azure.Arc.Tags["k"], "v")
	}
	copy.Azure.Arc.Tags["k"] = "copy"
	if cfg.Azure.Arc.Tags["k"] != "orig" {
		t.Fatalf("Arc.Tags shared; orig=%q, want %q", cfg.Azure.Arc.Tags["k"], "orig")
	}

	cfg.Node.Labels["l"] = "orig"
	if copy.Node.Labels["l"] != "1" {
		t.Fatalf("Node.Labels shared; copy=%q, want %q", copy.Node.Labels["l"], "1")
	}
	copy.Node.Labels["l"] = "copy"
	if cfg.Node.Labels["l"] != "orig" {
		t.Fatalf("Node.Labels shared; orig=%q, want %q", cfg.Node.Labels["l"], "orig")
	}

	cfg.Node.Kubelet.KubeReserved["cpu"] = "200m"
	if copy.Node.Kubelet.KubeReserved["cpu"] != "100m" {
		t.Fatalf("KubeReserved shared; copy=%q, want %q", copy.Node.Kubelet.KubeReserved["cpu"], "100m")
	}
	copy.Node.Kubelet.KubeReserved["cpu"] = "300m"
	if cfg.Node.Kubelet.KubeReserved["cpu"] != "200m" {
		t.Fatalf("KubeReserved shared; orig=%q, want %q", cfg.Node.Kubelet.KubeReserved["cpu"], "200m")
	}

	cfg.Node.Kubelet.EvictionHard["memory.available"] = "150Mi"
	if copy.Node.Kubelet.EvictionHard["memory.available"] != "200Mi" {
		t.Fatalf("EvictionHard shared; copy=%q, want %q", copy.Node.Kubelet.EvictionHard["memory.available"], "200Mi")
	}
	copy.Node.Kubelet.EvictionHard["memory.available"] = "250Mi"
	if cfg.Node.Kubelet.EvictionHard["memory.available"] != "150Mi" {
		t.Fatalf("EvictionHard shared; orig=%q, want %q", cfg.Node.Kubelet.EvictionHard["memory.available"], "150Mi")
	}
}
