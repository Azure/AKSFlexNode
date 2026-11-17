package config

import (
	"reflect"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"k8s.io/client-go/tools/clientcmd/api"
)

func TestIsolationDefaultsAndValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode  isolation.Mode
		want  isolation.Mode
		valid bool
	}{
		{"", isolation.ModeNSpawn, true}, {isolation.ModeNSpawn, isolation.ModeNSpawn, true}, {isolation.ModeNone, isolation.ModeNone, true}, {isolation.ModeContainer, isolation.ModeContainer, false}, {"unknown", "unknown", false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Bootstrap: BootstrapConfig{Isolation: tc.mode}}
			cfg.setDefaults()
			if cfg.Bootstrap.Isolation != tc.want {
				t.Fatalf("stored isolation = %q, want %q", cfg.Bootstrap.Isolation, tc.want)
			}
			if err := cfg.Bootstrap.validate(); (err == nil) != tc.valid {
				t.Fatalf("validation = %v, valid=%t", err, tc.valid)
			}
		})
	}
}

func TestIsolationAuthenticationMapping(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			for _, auth := range []struct {
				name                        string
				token, mi, sp, cert, paired bool
			}{
				{name: "token", token: true}, {name: "managed identity", mi: true}, {name: "service principal", sp: true}, {name: "certificate", sp: true, cert: true},
				{name: "token and identity", token: true, mi: true}, {name: "token and principal", token: true, sp: true}, {name: "token and certificate", token: true, sp: true, cert: true},
				{name: "paired", paired: true}, {name: "paired and token", paired: true, token: true}, {name: "paired and identity", paired: true, mi: true}, {name: "paired and principal", paired: true, sp: true}, {name: "paired and certificate", paired: true, sp: true, cert: true}, {name: "paired and token and identity", paired: true, token: true, mi: true},
			} {
				t.Run(auth.name, func(t *testing.T) {
					t.Parallel()
					cfg := &Config{Bootstrap: BootstrapConfig{Isolation: mode}, Agent: AgentConfig{NodeName: "node1"}, Azure: AzureConfig{TargetAgentPoolName: "flexpool"}, Components: ComponentsConfig{Kubernetes: "1.35.0"}, Node: NodeConfig{Labels: map[string]string{"custom": "value"}}}
					if auth.token {
						cfg.Azure.BootstrapToken = &BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"}
					}
					if auth.mi {
						cfg.Azure.ManagedIdentity = &ManagedIdentityConfig{ClientID: "identity"}
					}
					if auth.sp {
						cfg.Azure.ServicePrincipal = &ServicePrincipalConfig{ClientID: "principal", TenantID: "tenant", ClientSecret: "secret"}
						if auth.cert {
							cfg.Azure.ServicePrincipal.ClientSecret = ""
							cfg.Azure.ServicePrincipal.ClientSecretFile = "/etc/aks-flex-node/test-certificate.pem"
							cfg.Azure.ServicePrincipal.clientCertificatePEM = "certificate"
						}
					}
					if auth.paired {
						cfg.Agent.KubeconfigData = "agent credentials"
						cfg.Node.Kubelet.KubeconfigData = "worker credentials"
					}
					before := cfg.DeepCopy()
					got := ToAgentConfig(cfg, "kube1")
					if !reflect.DeepEqual(cfg, before) {
						t.Fatal("adapter mutated its input")
					}
					if got.Isolation != string(mode) || got.NodeName != cfg.Agent.NodeName || got.MachineName != "kube1" || got.Kubelet.Labels["custom"] != "value" {
						t.Fatal("mode or node configuration was not preserved")
					}
					if got.NSpawnLifecycle == nil || got.NSpawnLifecycle.PreStartCommand == "" {
						t.Fatal("missing SDK lifecycle configuration")
					}
					switch {
					case auth.paired:
						if string(got.Kubelet.KubeconfigData) != cfg.Node.Kubelet.KubeconfigData || got.Kubelet.Auth.ExecCredential != nil || got.Kubelet.Auth.BootstrapToken != "" {
							t.Fatal("paired credentials lost precedence")
						}
					case auth.token:
						if got.Kubelet.Auth.BootstrapToken != cfg.Azure.BootstrapToken.Token || got.Kubelet.Auth.ExecCredential != nil {
							t.Fatal("bootstrap token lost precedence")
						}
					default:
						command := "/usr/local/bin/aks-flex-node"
						if mode == isolation.ModeNone {
							command = "/opt/unbounded/agent/bin/aks-flex-node"
						}
						exec := got.Kubelet.Auth.ExecCredential
						if exec == nil || exec.Command != command || exec.APIVersion != "client.authentication.k8s.io/v1" || exec.InteractiveMode != api.NeverExecInteractiveMode || exec.ProvideClusterInfo {
							t.Fatal("incorrect credential command or exec settings")
						}
						args := []string{"token", "kubelogin", "--server-id", aksAADServerID}
						env := map[string]string{"AAD_LOGIN_METHOD": "msi", "AZURE_CLIENT_ID": "identity"}
						if auth.sp {
							env = map[string]string{"AAD_LOGIN_METHOD": "spn", "AAD_SERVICE_PRINCIPAL_CLIENT_ID": "principal", "AZURE_TENANT_ID": "tenant"}
							if auth.cert {
								args = append(args, "--client-certificate-file", cfg.Azure.ServicePrincipal.ClientSecretFile)
							} else {
								env["AAD_SERVICE_PRINCIPAL_CLIENT_SECRET"] = cfg.Azure.ServicePrincipal.ClientSecret
							}
						}
						actualEnv := map[string]string{}
						for _, variable := range exec.Env {
							actualEnv[variable.Name] = variable.Value
						}
						if !reflect.DeepEqual(exec.Args, args) || !reflect.DeepEqual(actualEnv, env) {
							t.Fatal("credential arguments or environment differ")
						}
					}
				})
			}
		})
	}
}
