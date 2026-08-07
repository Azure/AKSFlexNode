package config

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestToAgentConfig_BootstrapToken(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
		Components: ComponentsConfig{Kubernetes: "1.30.0"},
		Networking: NetworkingConfig{DNSServiceIP: "10.0.0.10"},
		Node: NodeConfig{
			Labels: map[string]string{"env": "test"},
			Taints: []string{"dedicated=infra:NoSchedule"},
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "dGVzdC1jYS1kYXRh",
				NodeIP:      "10.225.0.4",
			},
		},
	}

	cfg.Agent.NodeName = "test-node"
	ac := ToAgentConfig(cfg, "kube1")

	if ac.MachineName != "kube1" {
		t.Fatalf("MachineName=%q, want %q", ac.MachineName, "kube1")
	}
	if ac.NodeName != "test-node" {
		t.Fatalf("NodeName=%q, want test-node", ac.NodeName)
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
	if ac.Kubelet.NodeIP != "10.225.0.4" {
		t.Fatalf("Kubelet.NodeIP=%q, want %q", ac.Kubelet.NodeIP, "10.225.0.4")
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

func TestToAgentConfig_NodeName(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agent: AgentConfig{NodeName: "worker-1"}}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.NodeName != "worker-1" {
		t.Fatalf("NodeName=%q, want worker-1", ac.NodeName)
	}
}

func TestToAgentConfig_ResourceReservations(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Node: NodeConfig{
			MaxPods: 30,
			Kubelet: KubeletConfig{
				SystemReserved: map[string]string{"cpu": "50m", "memory": "100Mi"},
				KubeReserved:   map[string]string{"cpu": "200m", "memory": "650Mi"},
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	configuration := ac.Kubelet.Configuration
	gotSystemReserved, ok := configuration["systemReserved"].(map[string]string)
	if !ok || !maps.Equal(gotSystemReserved, cfg.Node.Kubelet.SystemReserved) {
		got := configuration["systemReserved"]
		t.Fatalf("systemReserved=%v, want %v", got, cfg.Node.Kubelet.SystemReserved)
	}
	gotKubeReserved, ok := configuration["kubeReserved"].(map[string]string)
	if !ok || !maps.Equal(gotKubeReserved, cfg.Node.Kubelet.KubeReserved) {
		got := configuration["kubeReserved"]
		t.Fatalf("kubeReserved=%v, want %v", got, cfg.Node.Kubelet.KubeReserved)
	}
	if got := configuration["maxPods"]; got != 30 {
		t.Fatalf("maxPods=%v, want 30", got)
	}
}

func TestDefaultKubeReserved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		cpuCount      int
		totalMemoryMi int
		maxPods       int
		wantCPU       string
		wantMemory    string
	}{
		{name: "one CPU", cpuCount: 1, totalMemoryMi: 8192, maxPods: 30, wantCPU: "60m", wantMemory: "650Mi"},
		{name: "two CPUs", cpuCount: 2, totalMemoryMi: 8192, maxPods: 30, wantCPU: "100m", wantMemory: "650Mi"},
		{name: "four CPUs", cpuCount: 4, totalMemoryMi: 8192, maxPods: 30, wantCPU: "140m", wantMemory: "650Mi"},
		{name: "eight CPUs", cpuCount: 8, totalMemoryMi: 8192, maxPods: 30, wantCPU: "180m", wantMemory: "650Mi"},
		{name: "sixteen CPUs", cpuCount: 16, totalMemoryMi: 8192, maxPods: 30, wantCPU: "260m", wantMemory: "650Mi"},
		{name: "memory capped at 25 percent", cpuCount: 2, totalMemoryMi: 2048, maxPods: 110, wantCPU: "100m", wantMemory: "512Mi"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := defaultKubeReserved(tt.cpuCount, tt.totalMemoryMi, tt.maxPods)
			if got["cpu"] != tt.wantCPU {
				t.Errorf("cpu=%q, want %q", got["cpu"], tt.wantCPU)
			}
			if got["memory"] != tt.wantMemory {
				t.Errorf("memory=%q, want %q", got["memory"], tt.wantMemory)
			}
		})
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
		Components: ComponentsConfig{Kubernetes: "1.31.0"},
		Networking: NetworkingConfig{DNSServiceIP: "10.0.0.10"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "ca-data",
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

func TestToAgentConfig_ServicePrincipalClientSecretFile(t *testing.T) {
	t.Parallel()

	clientSecretFile := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(clientSecretFile, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile: %v", err)
	}

	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{
				TenantID:         "tenant-123",
				ClientID:         "client-456",
				ClientSecretFile: clientSecretFile,
			},
		},
	}
	if err := cfg.Azure.ServicePrincipal.validate(); err != nil {
		t.Fatalf("ServicePrincipalConfig.validate() error = %v", err)
	}

	exec := ToAgentConfig(cfg, "kube1").Kubelet.Auth.ExecCredential
	if exec == nil {
		t.Fatal("ExecCredential should be set for SP auth")
	}
	envMap := make(map[string]string)
	for _, e := range exec.Env {
		envMap[e.Name] = e.Value
	}
	if envMap["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"] != "file-secret" {
		t.Fatalf("AAD_SERVICE_PRINCIPAL_CLIENT_SECRET=%q, want file secret", envMap["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"])
	}
}

func TestToAgentConfig_ServicePrincipalCertificateFile(t *testing.T) {
	t.Parallel()

	certificateFile := filepath.Join(t.TempDir(), "client-certificate.pem")
	writeTestClientCertificate(t, certificateFile)
	cfg := &Config{
		Azure: AzureConfig{
			ServicePrincipal: &ServicePrincipalConfig{
				TenantID:         "tenant-123",
				ClientID:         "client-456",
				ClientSecretFile: certificateFile,
			},
		},
	}
	if err := cfg.Azure.ServicePrincipal.validate(); err != nil {
		t.Fatalf("ServicePrincipalConfig.validate() error = %v", err)
	}

	exec := ToAgentConfig(cfg, "kube1").Kubelet.Auth.ExecCredential
	if exec == nil {
		t.Fatal("ExecCredential should be set for SP auth")
	}
	envMap := make(map[string]string)
	for _, e := range exec.Env {
		envMap[e.Name] = e.Value
	}
	if !slices.Equal(exec.Args, []string{"token", "kubelogin", "--server-id", aksAADServerID, "--client-certificate-file", certificateFile}) {
		t.Fatalf("Args=%v, want kubelogin client-certificate-file args", exec.Args)
	}
	if _, ok := envMap["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"]; ok {
		t.Fatal("AAD_SERVICE_PRINCIPAL_CLIENT_SECRET should not be set for certificate auth")
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
		Components: ComponentsConfig{Kubernetes: "1.31.0"},
		Networking: NetworkingConfig{DNSServiceIP: "10.0.0.10"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "ca-data",
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
		Azure: AzureConfig{
			ManagedIdentity: &ManagedIdentityConfig{},
		},
		Components: ComponentsConfig{Kubernetes: "1.31.0"},
		Networking: NetworkingConfig{DNSServiceIP: "10.0.0.10"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "ca-data",
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
		Components: ComponentsConfig{
			Kubernetes:   "1.30.0",
			Containerd:   "2.1.0",
			Runc:         "1.2.0",
			SandboxImage: "registry.example.test/pause:3.9",
		},
		Bootstrap: BootstrapConfig{
			OCIImage: "registry.example.test/flex/rootfs:ubuntu-24.04",
		},
		Networking: NetworkingConfig{
			DNSServiceIP: "10.0.0.10",
			CNIVersion:   "1.6.0",
		},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "ca",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	if ac.CRI.Containerd.Version != "2.1.0" {
		t.Fatalf("CRI.Containerd.Version=%q, want %q", ac.CRI.Containerd.Version, "2.1.0")
	}
	if ac.CRI.Containerd.SandboxImage != "registry.example.test/pause:3.9" {
		t.Fatalf("CRI.Containerd.SandboxImage=%q, want registry.example.test/pause:3.9", ac.CRI.Containerd.SandboxImage)
	}
	if ac.OCIImage != "registry.example.test/flex/rootfs:ubuntu-24.04" {
		t.Fatalf("OCIImage=%q, want registry.example.test/flex/rootfs:ubuntu-24.04", ac.OCIImage)
	}
	if ac.CRI.Runc.Version != "1.2.0" {
		t.Fatalf("CRI.Runc.Version=%q, want %q", ac.CRI.Runc.Version, "1.2.0")
	}
	if ac.CNI.PluginVersion != "1.6.0" {
		t.Fatalf("CNI.PluginVersion=%q, want %q", ac.CNI.PluginVersion, "1.6.0")
	}
}

func TestToAgentConfig_Gantry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		gantry       *GantryConfig
		wantConfig   bool
		wantDisabled bool
	}{
		{
			name: "omitted is enabled by default",
		},
		{
			name:       "explicitly enabled",
			gantry:     &GantryConfig{},
			wantConfig: true,
		},
		{
			name:         "disabled",
			gantry:       &GantryConfig{Disabled: true},
			wantConfig:   true,
			wantDisabled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Components: ComponentsConfig{Gantry: tt.gantry}}
			ac := ToAgentConfig(cfg, "kube1")

			if got := ac.Gantry != nil; got != tt.wantConfig {
				t.Fatalf("Gantry config present=%t, want %t", got, tt.wantConfig)
			}
			if got := goalstates.ResolveGantry(ac.Gantry).Disabled; got != tt.wantDisabled {
				t.Fatalf("Gantry.Disabled=%t, want %t", got, tt.wantDisabled)
			}
		})
	}
}

func TestToAgentConfig_OfflineArtifacts(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Bootstrap: BootstrapConfig{
			OfflineArtifacts: OfflineArtifactsConfig{Source: "/opt/artifacts/{{ .KubernetesVersion }}"},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")
	if ac.OfflineArtifacts == nil {
		t.Fatal("OfflineArtifacts is nil")
	}
	if ac.OfflineArtifacts.Source != "/opt/artifacts/{{ .KubernetesVersion }}" {
		t.Fatalf("OfflineArtifacts.Source=%q", ac.OfflineArtifacts.Source)
	}
}

func TestToAgentConfig_AdditionalHostDevices(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Bootstrap: BootstrapConfig{
			AdditionalHostDevices: []string{"/dev/uinput", "/dev/input/event0"},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")
	if len(ac.AdditionalHostDevices) != 2 {
		t.Fatalf("AdditionalHostDevices=%#v, want 2 entries", ac.AdditionalHostDevices)
	}
	if ac.AdditionalHostDevices[0] != "/dev/uinput" || ac.AdditionalHostDevices[1] != "/dev/input/event0" {
		t.Fatalf("AdditionalHostDevices=%#v", ac.AdditionalHostDevices)
	}
}

func TestToAgentConfig_AdditionalHostMounts(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Bootstrap: BootstrapConfig{
			AdditionalHostMounts: []AdditionalHostMount{
				{Source: "/opt/config", Target: "/etc/config", ReadOnly: true},
				{Source: "/var/lib/example"},
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")
	if len(ac.AdditionalHostMounts) != 2 {
		t.Fatalf("AdditionalHostMounts=%#v, want 2 entries", ac.AdditionalHostMounts)
	}
	if got := ac.AdditionalHostMounts[0]; got.Source != "/opt/config" || got.Target != "/etc/config" || !got.ReadOnly {
		t.Fatalf("AdditionalHostMounts[0]=%#v", got)
	}
	if got := ac.AdditionalHostMounts[1]; got.Source != "/var/lib/example" || got.Target != "" || got.ReadOnly {
		t.Fatalf("AdditionalHostMounts[1]=%#v", got)
	}
}

func TestToAgentConfig_CRICNIVersionsEmpty(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Azure: AzureConfig{
			BootstrapToken: &BootstrapTokenConfig{Token: "tok"},
		},
		Components: ComponentsConfig{Kubernetes: "1.30.0"},
		Networking: NetworkingConfig{DNSServiceIP: "10.0.0.10"},
		Node: NodeConfig{
			Kubelet: KubeletConfig{
				ClusterFQDN: "api.example.com:6443",
				CACertData:  "ca",
			},
		},
	}

	ac := ToAgentConfig(cfg, "kube1")

	// Empty values should be passed through; the library defaults in
	// goalstates.ResolveMachine when empty.
	if ac.CRI.Containerd.Version != "" {
		t.Fatalf("CRI.Containerd.Version=%q, want empty", ac.CRI.Containerd.Version)
	}
	if ac.CRI.Containerd.SandboxImage != "" {
		t.Fatalf("CRI.Containerd.SandboxImage=%q, want empty", ac.CRI.Containerd.SandboxImage)
	}
	if ac.OCIImage != "" {
		t.Fatalf("OCIImage=%q, want empty", ac.OCIImage)
	}
	if ac.CRI.Runc.Version != "" {
		t.Fatalf("CRI.Runc.Version=%q, want empty", ac.CRI.Runc.Version)
	}
	if ac.CNI.PluginVersion != "" {
		t.Fatalf("CNI.PluginVersion=%q, want empty", ac.CNI.PluginVersion)
	}
}
