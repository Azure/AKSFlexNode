package bootstrap

import (
	"slices"
	"strings"
	"testing"

	bootstrapflow "github.com/Azure/AKSFlexNode/pkg/bootstrap"
)

func TestNewCommand(t *testing.T) {
	t.Parallel()
	command := NewCommand()
	if command.Use != "boot" {
		t.Fatalf("Use = %q, want boot", command.Use)
	}
	for _, name := range []string{
		"auth", "msi-client-id", "sp-tenant-id", "sp-client-id",
		"sp-client-secret-file", "sp-client-certificate-file",
		"fetch-bootstrap-data", "cluster-resource-id", "agent-pool-name",
		"resource-manager-endpoint", "bootstrap-oci-image",
		"bootstrap-offline-artifacts-source", "config-overrides", "config",
		"config-path", "agent-url", "agent-version", "agent-sha256", "install-dir",
	} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
}

func TestReexecuteEnvironmentPreservesDirectSecret(t *testing.T) {
	t.Parallel()
	got := reexecuteEnvironment([]string{
		"KEEP=value",
		"AKS_FLEX_NODE_AGENT_UPDATE_APPLIED=old",
		"AKS_FLEX_NODE_SP_CLIENT_SECRET=old",
	}, bootstrapflow.Options{SPClientSecret: "new-secret"})
	for _, want := range []string{
		"KEEP=value",
		"AKS_FLEX_NODE_AGENT_UPDATE_APPLIED=1",
		"AKS_FLEX_NODE_SP_CLIENT_SECRET=new-secret",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("environment %v does not contain %q", got, want)
		}
	}
	if slices.Contains(got, "AKS_FLEX_NODE_SP_CLIENT_SECRET=old") {
		t.Fatal("old secret was preserved")
	}
}

func TestWithoutEnvironment(t *testing.T) {
	t.Parallel()
	got := withoutEnvironment([]string{"KEEP=value", "SECRET=value", "URL=value"}, "SECRET=", "URL=")
	if strings.Join(got, ",") != "KEEP=value" {
		t.Fatalf("withoutEnvironment() = %v", got)
	}
}
