package config

import (
	"testing"
)

func TestToAgentConfig_BootstrapToken(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
		Kubernetes: KubernetesConfig{Version: "1.30.0"},
		Node: NodeConfig{
			Labels: map[string]string{"env": "test"},
			Taints: []string{"dedicated=infra:NoSchedule"},
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "dGVzdC1jYS1kYXRh",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.MachineName != "kube1" {
		t.Fatalf("MachineName=%q, want %q", ac.MachineName, "kube1")
	}
	if ac.Cluster.Version != "1.30.0" {
		t.Fatalf("Cluster.Version=%q, want %q", ac.Cluster.Version, "1.30.0")
	}
	if ac.Cluster.ClusterDNS != "10.0.0.10" {
		t.Fatalf("Cluster.ClusterDNS=%q, want %q", ac.Cluster.ClusterDNS, "10.0.0.10")
	}
	if ac.Cluster.CaCertBase64 != "dGVzdC1jYS1kYXRh" {
		t.Fatalf("Cluster.CaCertBase64=%q, want %q", ac.Cluster.CaCertBase64, "dGVzdC1jYS1kYXRh")
	}
	if ac.Kubelet.ApiServer != "https://api.example.com:6443" {
		t.Fatalf("Kubelet.ApiServer=%q, want %q", ac.Kubelet.ApiServer, "https://api.example.com:6443")
	}
	if ac.Kubelet.Auth.BootstrapToken != "abcdef.0123456789abcdef" {
		t.Fatalf("Kubelet.Auth.BootstrapToken=%q, want %q", ac.Kubelet.Auth.BootstrapToken, "abcdef.0123456789abcdef")
	}
	if ac.Kubelet.Auth.ExecCredential != nil {
		t.Fatalf("Kubelet.Auth.ExecCredential should be nil for bootstrap token auth")
	}
	if len(ac.Kubelet.Labels) != 1 || ac.Kubelet.Labels["env"] != "test" {
		t.Fatalf("Kubelet.Labels=%v, want map[env:test]", ac.Kubelet.Labels)
	}
	if len(ac.Kubelet.RegisterWithTaints) != 1 || ac.Kubelet.RegisterWithTaints[0] != "dedicated=infra:NoSchedule" {
		t.Fatalf("Kubelet.RegisterWithTaints=%v, want [dedicated=infra:NoSchedule]", ac.Kubelet.RegisterWithTaints)
	}
}

func TestToAgentConfig_ServicePrincipal(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{
				TenantID:     "tenant-123",
				ClientID:     "client-456",
				ClientSecret: "secret-789",
			},
		},
		Kubernetes: KubernetesConfig{Version: "1.31.0"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca-data",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.Kubelet.Auth.BootstrapToken != "" {
		t.Fatalf("BootstrapToken should be empty for SP auth, got %q", ac.Kubelet.Auth.BootstrapToken)
	}
	if ac.Kubelet.Auth.ExecCredential == nil {
		t.Fatal("ExecCredential should be set for SP auth")
	}

	exec := ac.Kubelet.Auth.ExecCredential
	if exec.Command != flexNodeBinaryPath {
		t.Fatalf("Command=%q, want %q", exec.Command, flexNodeBinaryPath)
	}
	if exec.APIVersion != "client.authentication.k8s.io/v1" {
		t.Fatalf("APIVersion=%q, want %q", exec.APIVersion, "client.authentication.k8s.io/v1")
	}

	envMap := make(map[string]string)
	for _, e := range exec.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AAD_LOGIN_METHOD"] != "spn" {
		t.Fatalf("AAD_LOGIN_METHOD=%q, want %q", envMap["AAD_LOGIN_METHOD"], "spn")
	}
	if envMap["AAD_SERVICE_PRINCIPAL_CLIENT_ID"] != "client-456" {
		t.Fatalf("AAD_SERVICE_PRINCIPAL_CLIENT_ID=%q, want %q", envMap["AAD_SERVICE_PRINCIPAL_CLIENT_ID"], "client-456")
	}
	if envMap["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"] != "secret-789" {
		t.Fatalf("AAD_SERVICE_PRINCIPAL_CLIENT_SECRET=%q, want %q", envMap["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"], "secret-789")
	}
	if envMap["AZURE_TENANT_ID"] != "tenant-123" {
		t.Fatalf("AZURE_TENANT_ID=%q, want %q", envMap["AZURE_TENANT_ID"], "tenant-123")
	}
}

func TestToAgentConfig_ManagedIdentity(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			ManagedIdentity: &ManagedIdentityConfig{
				ClientID: "mi-client-id",
			},
		},
		Kubernetes:        KubernetesConfig{Version: "1.31.0"},
		isMIExplicitlySet: true,
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca-data",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube2")

	if ac.Kubelet.Auth.BootstrapToken != "" {
		t.Fatalf("BootstrapToken should be empty for MSI auth, got %q", ac.Kubelet.Auth.BootstrapToken)
	}
	if ac.Kubelet.Auth.ExecCredential == nil {
		t.Fatal("ExecCredential should be set for MSI auth")
	}

	exec := ac.Kubelet.Auth.ExecCredential
	if exec.Command != flexNodeBinaryPath {
		t.Fatalf("Command=%q, want %q", exec.Command, flexNodeBinaryPath)
	}

	envMap := make(map[string]string)
	for _, e := range exec.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AAD_LOGIN_METHOD"] != "msi" {
		t.Fatalf("AAD_LOGIN_METHOD=%q, want %q", envMap["AAD_LOGIN_METHOD"], "msi")
	}
	if envMap["AZURE_CLIENT_ID"] != "mi-client-id" {
		t.Fatalf("AZURE_CLIENT_ID=%q, want %q", envMap["AZURE_CLIENT_ID"], "mi-client-id")
	}
	if ac.MachineName != "kube2" {
		t.Fatalf("MachineName=%q, want %q", ac.MachineName, "kube2")
	}
}

func TestToAgentConfig_ManagedIdentitySystemAssigned(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure:             AzureConfig{},
		isMIExplicitlySet: true,
		Kubernetes:        KubernetesConfig{Version: "1.31.0"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca-data",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.Kubelet.Auth.ExecCredential == nil {
		t.Fatal("ExecCredential should be set for system-assigned MSI")
	}

	envMap := make(map[string]string)
	for _, e := range ac.Kubelet.Auth.ExecCredential.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AAD_LOGIN_METHOD"] != "msi" {
		t.Fatalf("AAD_LOGIN_METHOD=%q, want %q", envMap["AAD_LOGIN_METHOD"], "msi")
	}
	if _, hasClientID := envMap["AZURE_CLIENT_ID"]; hasClientID {
		t.Fatal("AZURE_CLIENT_ID should not be set for system-assigned MSI")
	}
}

func TestToAgentConfig_CRICNIVersions(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "tok"},
		},
		Kubernetes: KubernetesConfig{Version: "1.30.0"},
		Containerd: ContainerdConfig{Version: "2.1.0"},
		Runc:       RuncConfig{Version: "1.2.0"},
		CNI:        CNIConfig{Version: "1.6.0"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.CRI.Containerd.Version != "2.1.0" {
		t.Fatalf("CRI.Containerd.Version=%q, want %q", ac.CRI.Containerd.Version, "2.1.0")
	}
	if ac.CRI.Runc.Version != "1.2.0" {
		t.Fatalf("CRI.Runc.Version=%q, want %q", ac.CRI.Runc.Version, "1.2.0")
	}
	if ac.CNI.PluginVersion != "1.6.0" {
		t.Fatalf("CNI.PluginVersion=%q, want %q", ac.CNI.PluginVersion, "1.6.0")
	}
}

func TestToAgentConfig_CRICNIVersionsEmpty(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "tok"},
		},
		Kubernetes: KubernetesConfig{Version: "1.30.0"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				DNSServiceIP: "10.0.0.10",
				ServerURL:    "https://api.example.com:6443",
				CACertData:   "ca",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	// Empty values should be passed through; the library defaults in
	// goalstates.ResolveMachine when empty.
	if ac.CRI.Containerd.Version != "" {
		t.Fatalf("CRI.Containerd.Version=%q, want empty", ac.CRI.Containerd.Version)
	}
	if ac.CRI.Runc.Version != "" {
		t.Fatalf("CRI.Runc.Version=%q, want empty", ac.CRI.Runc.Version)
	}
	if ac.CNI.PluginVersion != "" {
		t.Fatalf("CNI.PluginVersion=%q, want empty", ac.CNI.PluginVersion)
	}
}
