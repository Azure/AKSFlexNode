package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/nodebootstrap"
)

const testPartialConfig = `{
  "azure": {
    "bootstrapToken": {"token": "abcdef.0123456789abcdef"},
    "arc": {"enabled": false},
    "targetCluster": {
      "resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
      "location": "eastus"
    }
  },
  "components": {"kubernetes": "1.35.0"},
  "node": {"kubelet": {"clusterFQDN": "cluster.example.test:443", "caCertData": "Y2E="}}
}`

func TestNewCommand(t *testing.T) {
	t.Parallel()

	command := NewCommand()
	if command.Use != "bootstrap" {
		t.Fatalf("Use = %q, want bootstrap", command.Use)
	}
	for _, name := range []string{
		"start-config-url",
		"agent-binary-url",
		"storage-auth",
		"storage-client-secret-file",
		"jq",
		"jq-arg",
		"jq-argjson",
	} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
}

func TestNewCommandRedactsURLDefaults(t *testing.T) {
	t.Setenv("AKS_FLEX_NODE_START_CONFIG_URL", "https://account.example/config?sig=start-secret")
	t.Setenv("AKS_FLEX_NODE_AGENT_BINARY_URL", "https://account.example/agent?sig=agent-secret")
	command := NewCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Help(); err != nil {
		t.Fatalf("Help() error = %v", err)
	}
	for _, secret := range []string{"start-secret", "agent-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("help output contains URL credential %q", secret)
		}
	}
}

func TestHandlerExecuteRendersPreflightsAndStarts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	source := filepath.Join(dir, "partial.json")
	if err := os.WriteFile(source, []byte(testPartialConfig), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	configPath := filepath.Join(dir, "etc", "config.json")
	var calls [][]string
	h := &handler{
		options: options{
			startConfigURL: source,
			configPath:     configPath,
			storageAuth:    "none",
			jqQueries:      []string{`.node.maxPods = $pods`},
			jqJSONArgs:     []string{"pods=200"},
		},
		executable: func() (string, error) { return "/fake/aks-flex-node", nil },
		reexec:     func(string) error { t.Fatal("unexpected re-exec"); return nil },
		runCommand: func(_ context.Context, path string, arguments ...string) error {
			calls = append(calls, append([]string{path}, arguments...))
			return nil
		},
		writeConfig:   writeConfig,
		newDownloader: nodebootstrap.NewDownloader,
	}
	if err := h.execute(context.Background()); err != nil {
		t.Fatalf("execute() error = %v", err)
	}

	wantCalls := [][]string{
		{"/fake/aks-flex-node", "preflight", "--config", configPath, "--output", "text"},
		{"/fake/aks-flex-node", "start", "--config", configPath},
	}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("commands = %#v, want %#v", calls, wantCalls)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestHandlerExecuteExistingConfigCompatibility(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
	var calls [][]string
	h := &handler{
		options:    options{configPath: path, storageAuth: "none"},
		executable: func() (string, error) { return "/fake/aks-flex-node", nil },
		runCommand: func(_ context.Context, executable string, arguments ...string) error {
			calls = append(calls, append([]string{executable}, arguments...))
			return nil
		},
		newDownloader: nodebootstrap.NewDownloader,
	}
	if err := h.execute(context.Background()); err != nil {
		t.Fatalf("execute() error = %v", err)
	}
	want := [][]string{{"/fake/aks-flex-node", "start", "--config", path}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("commands = %#v, want %#v", calls, want)
	}
}
