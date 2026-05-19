package config

import "testing"

func TestConfigDeepCopy_DoesNotSharePointersOrMaps(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{TenantID: "t", ClientID: "c", ClientSecret: "s"},
			ManagedIdentity:  &ManagedIdentityConfig{ClientID: "mi"},
			BootstrapToken:   &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
			TargetCluster:    &TargetClusterConfig{ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster", Location: "eastus"},
			Arc:              &ArcConfig{Enabled: true, MachineName: "m", Location: "eastus", ResourceGroup: "rg", Tags: map[string]string{"k": "v"}},
		},
		Node: NodeConfig{
			Labels: map[string]string{"l": "1"},
			Taints: []string{"dedicated=infra:NoSchedule"},
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

	// Taints slice should not be shared.
	if len(copy.Node.Taints) != 1 || copy.Node.Taints[0] != "dedicated=infra:NoSchedule" {
		t.Fatalf("Node.Taints copy=%v, want [dedicated=infra:NoSchedule]", copy.Node.Taints)
	}
	cfg.Node.Taints[0] = "mutated:NoExecute"
	if copy.Node.Taints[0] != "dedicated=infra:NoSchedule" {
		t.Fatalf("Node.Taints shares backing array; copy=%q, want %q", copy.Node.Taints[0], "dedicated=infra:NoSchedule")
	}

}
