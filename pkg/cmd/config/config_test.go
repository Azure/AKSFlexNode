package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type runnerCall struct {
	name  string
	args  []string
	input string
}

type fakeRunner struct {
	responses []string
	calls     []runnerCall
	missing   map[string]bool
}

func (runner *fakeRunner) LookPath(name string) error {
	if runner.missing[name] {
		return errors.New("not found")
	}
	return nil
}

func (runner *fakeRunner) Run(_ context.Context, name string, args []string, input string) (string, error) {
	runner.calls = append(runner.calls, runnerCall{name: name, args: append([]string(nil), args...), input: input})
	if len(runner.responses) == 0 {
		return "", nil
	}
	response := runner.responses[0]
	runner.responses = runner.responses[1:]
	return response, nil
}

func TestRootCommandRegistersCommands(t *testing.T) {
	t.Parallel()

	cmd := newCommand(testDependencies(&fakeRunner{}))
	for _, name := range []string{"setup-node-rbac", "generate-node-config"} {
		found, _, err := cmd.Find([]string{name})
		if err != nil {
			t.Fatalf("Find(%q) error = %v", name, err)
		}
		if found.Name() != name {
			t.Fatalf("Find(%q) command = %q", name, found.Name())
		}
	}
}

func TestSelectedAuthModeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		options generateOptions
		want    string
		wantErr string
	}{
		{name: "bootstrap token", options: generateOptions{bootstrapToken: true}, want: "bootstrap-token"},
		{name: "identity", options: generateOptions{identity: true}, want: "identity"},
		{name: "service principal", options: generateOptions{servicePrincipal: true, username: "client", password: "secret"}, want: "service-principal"},
		{name: "password implies service principal", options: generateOptions{username: "client", password: "secret"}, want: "service-principal"},
		{name: "none", options: generateOptions{}, wantErr: "choose exactly one"},
		{name: "multiple", options: generateOptions{bootstrapToken: true, identity: true}, wantErr: "choose exactly one"},
		{name: "missing client", options: generateOptions{servicePrincipal: true, password: "secret"}, wantErr: "requires --username"},
		{name: "arc", options: generateOptions{arc: true}, wantErr: "must be fetched on the connected host"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectedAuthMode(test.options)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("selectedAuthMode() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("selectedAuthMode() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("selectedAuthMode() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestGenerateBootstrapTokenConfig(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{responses: []string{
		"sub-id", "tenant-id", "/subscriptions/sub-id/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster",
		"westus", "1.34.3", "10.0.0.10", "", "", "https://cluster.example:443", "Y2E=",
	}}
	deps := testDependencies(runner)
	var stdout bytes.Buffer
	deps.stdout = &stdout
	options := generateOptions{
		clusterOptions: clusterOptions{resourceGroup: "rg", clusterName: "cluster", subscription: "sub-id"},
		agentPoolName:  defaultAgentPoolName,
		bootstrapToken: true,
		output:         "-",
	}

	if err := generateNodeConfig(context.Background(), deps, options); err != nil {
		t.Fatalf("generateNodeConfig() error = %v", err)
	}

	var config map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &config); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	azure := config["azure"].(map[string]any)
	if got := azure["targetAgentPoolName"]; got != defaultAgentPoolName {
		t.Fatalf("targetAgentPoolName = %v, want %q", got, defaultAgentPoolName)
	}
	bootstrap := azure["bootstrapToken"].(map[string]any)
	if got := bootstrap["token"]; got != "000102.030405060708090a" {
		t.Fatalf("bootstrap token = %v", got)
	}
	node := config["node"].(map[string]any)
	kubelet := node["kubelet"].(map[string]any)
	if got := kubelet["clusterFQDN"]; got != "cluster.example:443" {
		t.Fatalf("clusterFQDN = %v", got)
	}

	var tokenManifest string
	for _, call := range runner.calls {
		if call.name == "kubectl" && len(call.args) > 0 && call.args[0] == "apply" && strings.Contains(call.input, "kind: Secret") {
			tokenManifest = call.input
		}
	}
	for _, value := range []string{"bootstrap-token-000102", `token-secret: "030405060708090a"`, `expiration: "2026-08-21T12:00:00Z"`} {
		if !strings.Contains(tokenManifest, value) {
			t.Errorf("token manifest missing %q:\n%s", value, tokenManifest)
		}
	}
}

func TestSetupNodeRBACAppliesManifest(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	deps := testDependencies(runner)
	options := clusterOptions{resourceGroup: "rg", clusterName: "cluster", subscription: "sub"}
	if err := setupNodeRBAC(context.Background(), deps, options); err != nil {
		t.Fatalf("setupNodeRBAC() error = %v", err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(runner.calls))
	}
	if !strings.Contains(runner.calls[1].input, "aks-flex-node-auto-approve-csr") {
		t.Fatalf("RBAC manifest was not passed to kubectl")
	}
}

func TestRenderConfigAuthModes(t *testing.T) {
	t.Parallel()

	metadata := map[string]string{
		"subscription_id":    "sub-id",
		"tenant_id":          "tenant-id",
		"resource_id":        "cluster-id",
		"location":           "westus",
		"kubernetes_version": "1.34.3",
		"agent_pool_name":    "pool",
	}
	tests := []struct {
		name       string
		mode       string
		options    generateOptions
		azureKey   string
		wantValues map[string]any
	}{
		{name: "system assigned identity", mode: "identity", options: generateOptions{}, azureKey: "managedIdentity", wantValues: map[string]any{}},
		{name: "user assigned identity", mode: "identity", options: generateOptions{username: "identity-client"}, azureKey: "managedIdentity", wantValues: map[string]any{"clientId": "identity-client"}},
		{name: "service principal defaults tenant", mode: "service-principal", options: generateOptions{username: "client", password: "secret"}, azureKey: "servicePrincipal", wantValues: map[string]any{"tenantId": "tenant-id", "clientId": "client", "clientSecret": "secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, err := renderConfig(context.Background(), testDependencies(&fakeRunner{}), test.options, test.mode, metadata)
			if err != nil {
				t.Fatalf("renderConfig() error = %v", err)
			}
			azure := config["azure"].(map[string]any)
			got := azure[test.azureKey].(map[string]any)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(test.wantValues)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("%s = %s, want %s", test.azureKey, gotJSON, wantJSON)
			}
		})
	}
}

func TestWriteConfigFile(t *testing.T) {
	t.Parallel()

	output := filepath.Join(t.TempDir(), "nested", "config.json")
	if err := writeConfig(testDependencies(&fakeRunner{}), map[string]any{"value": "test"}, output); err != nil {
		t.Fatalf("writeConfig() error = %v", err)
	}
	rendered, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("os.ReadFile() error = %v", err)
	}
	if got, want := string(rendered), "{\n  \"value\": \"test\"\n}\n"; got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("os.Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
}

func TestMissingRequiredCommand(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{missing: map[string]bool{"kubectl": true}}
	err := setupNodeRBAC(context.Background(), testDependencies(runner), clusterOptions{})
	if err == nil || err.Error() != "missing required command: kubectl" {
		t.Fatalf("setupNodeRBAC() error = %v", err)
	}
}

func testDependencies(runner commandRunner) dependencies {
	return dependencies{
		runner: runner,
		now: func() time.Time {
			return time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
		},
		random: bytes.NewReader([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}),
		stdout: &bytes.Buffer{},
		stderr: &bytes.Buffer{},
	}
}
