package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestBootstrapCredentialRESTConfig(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.BootstrapToken = &config.BootstrapTokenConfig{Token: "token.value"}

	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapCredentialRESTConfig: %v", err)
	}
	if restCfg.Host != cfg.Node.Kubelet.ClusterFQDN || restCfg.BearerToken != cfg.Azure.BootstrapToken.Token {
		t.Fatalf("rest config = %#v", restCfg)
	}
}

func TestBootstrapCredentialRESTConfigExecCredential(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))
	cfg.Azure.ServicePrincipal = &config.ServicePrincipalConfig{TenantID: "tenant", ClientID: "client", ClientSecret: "secret"}
	cfg.Components.Kubernetes = "1.34.0"

	restCfg, err := bootstrapCredentialRESTConfig(cfg)
	if err != nil {
		t.Fatalf("bootstrapCredentialRESTConfig: %v", err)
	}
	if restCfg.ExecProvider == nil {
		t.Fatalf("ExecProvider = nil, want exec credential")
	}
	if restCfg.ExecProvider.Command != "/usr/local/bin/aks-flex-node" {
		t.Fatalf("ExecProvider.Command = %q", restCfg.ExecProvider.Command)
	}
}

func TestBootstrapCredentialRESTConfigRequiresCredential(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	cfg.Node.Kubelet.ClusterFQDN = "https://example.test"
	cfg.Node.Kubelet.CACertData = base64.StdEncoding.EncodeToString([]byte("ca"))

	_, err := bootstrapCredentialRESTConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "exec credential") {
		t.Fatalf("error = %v, want credential error", err)
	}
}

func TestDaemonControllerCertificateOptions(t *testing.T) {
	t.Parallel()

	opts := daemonControllerCertificateOptions(filepath.Join(t.TempDir(), "creds"))
	if opts.Name != "aks-flex-node-daemon" {
		t.Fatalf("Name = %q, want aks-flex-node-daemon", opts.Name)
	}
	if opts.DaemonGroup != "aks-flex-node-daemons" {
		t.Fatalf("DaemonGroup = %q, want aks-flex-node-daemons", opts.DaemonGroup)
	}
	if opts.CredentialDir == "" {
		t.Fatal("CredentialDir is empty")
	}
	if opts.WaitTimeout == 0 {
		t.Fatal("WaitTimeout is empty")
	}
}

func TestOpenGateAfterAgentUpgradeJudgment(t *testing.T) {
	t.Parallel()

	judged := &agentUpgradeSignal{OperationName: "upgrade", Failure: "the candidate failed"}
	tests := map[string]struct {
		publishErrs []error
		signal      *agentUpgradeSignal
		convergeErr error
		want        []string
	}{
		"converges the held assets, then opens the gate, once the upgrade is judged": {
			want: []string{"publish", "converge", "open"},
		},
		"keeps the gate, and the assets, while recovery restarts into the last-good binary": {
			signal: judged,
			want:   []string{"publish"},
		},
		"retries publishing the result": {
			publishErrs: []error{errors.New("API server unavailable")},
			want:        []string{"publish", "publish", "converge", "open"},
		},
		// The last-good version's assets still work, and the next start converges them again.
		"opens the gate though it couldn't update the assets": {
			convergeErr: errors.New("read-only file system"),
			want:        []string{"publish", "converge", "open"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls []string
			publishes := 0
			openGateAfterAgentUpgradeJudgment(
				t.Context(),
				discardLogger(),
				func(context.Context) error {
					calls = append(calls, "publish")
					publishes++
					if publishes <= len(tc.publishErrs) {
						return tc.publishErrs[publishes-1]
					}
					return nil
				},
				func() (*agentUpgradeSignal, error) { return tc.signal, nil },
				func(context.Context) error {
					calls = append(calls, "converge")
					return tc.convergeErr
				},
				func() { calls = append(calls, "open") },
				time.Millisecond,
			)
			if !slices.Equal(calls, tc.want) {
				t.Errorf("calls = %q, want %q", calls, tc.want)
			}
		})
	}
}
