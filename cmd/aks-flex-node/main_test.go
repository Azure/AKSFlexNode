package main

import (
	"os"
	"path/filepath"
	"testing"

	preflightcmd "github.com/Azure/AKSFlexNode/pkg/cmd/preflight"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestMalformedKubeconfigExitCode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	data := `{
		"azure": {"targetCluster": {
			"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
			"location": "eastus"
		}},
		"agent": {"kubeconfigData": "apiVersion: ["},
		"node": {"kubelet": {"kubeconfigData": "apiVersion: ["}}
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := preflightcmd.NewCommand()
	cmd.SetArgs([]string{"--config", path})
	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("preflight accepted malformed kubeconfigs")
	}
	if got := commandErrorExitCode(err); got != 78 {
		t.Fatalf("commandErrorExitCode(%v) = %d, want 78", err, got)
	}
}

func TestConnectivityFailureExitCode(t *testing.T) {
	t.Parallel()

	report := preflight.Report{
		Checks:  preflight.ResultsError("api-server-reachable", "api", "connection refused"),
		Summary: preflight.Summary{Errors: 1},
	}
	err := report.Err(false)
	if err == nil {
		t.Fatal("connectivity failure did not return an error")
	}
	if got := commandErrorExitCode(err); got != 1 {
		t.Fatalf("commandErrorExitCode(%v) = %d, want retryable status 1", err, got)
	}
}

func TestRootCommandRegistersGeneratedNSpawnLifecycleShape(t *testing.T) {
	t.Parallel()

	cmd, remaining, err := newRootCommand().Find([]string{"nspawn-lifecycle", "pre-start", "kube1"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if cmd.Name() != "pre-start" {
		t.Fatalf("Find() command = %q, want pre-start", cmd.Name())
	}
	if len(remaining) != 1 || remaining[0] != "kube1" {
		t.Fatalf("Find() remaining args = %v, want [kube1]", remaining)
	}
}
