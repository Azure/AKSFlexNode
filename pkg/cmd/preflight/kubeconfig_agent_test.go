package preflight

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestPairedKubeconfigChecksReplaceUpstreamReachability(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	upstream := nodestart.Preflight(log, agentconfig.AgentConfig{}, &goalstates.MachineGoalState{
		NodeStart: &goalstates.NodeStart{},
	})
	cfg := &config.Config{Node: config.NodeConfig{Kubelet: config.KubeletConfig{KubeconfigData: "kubelet-data"}}}
	checks, err := pairedKubeconfigChecks(cfg, upstream)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != len(upstream)+1 {
		t.Fatalf("got %d checks, want %d", len(checks), len(upstream)+1)
	}
	replaced := 0
	for _, check := range checks {
		if check.Name() == checkAPIServerReachableName {
			replacement, ok := check.(kubeconfigAPIServerReachableChecker)
			if !ok || replacement.data != cfg.Node.Kubelet.KubeconfigData {
				t.Fatalf("API reachability check = %#v, want kubelet kubeconfig checker", check)
			}
			replaced++
		}
	}
	if replaced != 1 {
		t.Fatalf("replaced %d reachability checks, want 1", replaced)
	}
	if host, ok := checks[len(checks)-1].(agentKubeconfigChecker); !ok || host.cfg != cfg {
		t.Fatal("host agent credential check was not registered")
	}
}

func TestPairedKubeconfigChecksRejectUpstreamRename(t *testing.T) {
	t.Parallel()

	checks, err := pairedKubeconfigChecks(&config.Config{}, []preflight.Checker{renamedReachabilityChecker{}})
	if err == nil || !strings.Contains(err.Error(), checkAPIServerReachableName) {
		t.Fatalf("error = %v, want missing upstream check diagnostic", err)
	}
	if checks != nil {
		t.Fatal("returned usable checks despite missing the reachability replacement")
	}
}

type renamedReachabilityChecker struct{}

func (renamedReachabilityChecker) Name() string { return "upstream-renamed-api-check" }
func (renamedReachabilityChecker) Check(context.Context) []preflight.Result {
	return nil
}

func TestAgentKubeconfigChecker(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mode      os.FileMode
		missing   bool
		directory bool
		tokenOnly bool
		wantError bool
	}{
		"executable without invocation": {mode: 0o700},
		"missing executable":            {missing: true, wantError: true},
		"not executable":                {mode: 0o600, wantError: true},
		"directory":                     {directory: true, wantError: true},
		"no exec provider":              {tokenOnly: true},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			plugin := filepath.Join(dir, "host-plugin")
			marker := filepath.Join(dir, "called")
			if tt.directory {
				if err := os.Mkdir(plugin, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if !tt.missing {
				if err := os.WriteFile(plugin, fmt.Appendf(nil, "#!/bin/sh\ntouch %q\nexit 1\n", marker), tt.mode); err != nil {
					t.Fatal(err)
				}
			}
			credential := fmt.Sprintf(`exec:
      apiVersion: client.authentication.k8s.io/v1
      command: %s
      interactiveMode: Never`, plugin)
			if tt.tokenOnly {
				credential = "token: test-token"
			}
			data := `apiVersion: v1
kind: Config
current-context: default
clusters:
- name: cluster
  cluster:
    server: https://cluster.example
users:
- name: agent
  user:
    ` + credential + `
contexts:
- name: default
  context:
    cluster: cluster
    user: agent
`
			cfg := &config.Config{
				Agent: config.AgentConfig{KubeconfigData: data},
				Node: config.NodeConfig{Kubelet: config.KubeletConfig{
					KubeconfigData: strings.Replace(data, plugin, filepath.Join(dir, "container-only-plugin"), 1),
				}},
			}
			results := (agentKubeconfigChecker{cfg: cfg}).Check(t.Context())
			if len(results) != 1 || results[0].Name != checkAgentKubeconfigName {
				t.Fatalf("Check() = %#v, want one host agent result", results)
			}
			if tt.wantError {
				if results[0].Severity != preflight.SeverityError ||
					!strings.Contains(results[0].Message, "not executable on the host") {
					t.Fatalf("Check() = %#v, want host exec error", results)
				}
			} else if results[0].Severity != preflight.SeverityOK {
				t.Fatalf("Check() = %#v, want success", results)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("credential plugin ran during preflight: %v", err)
			}
		})
	}
}
