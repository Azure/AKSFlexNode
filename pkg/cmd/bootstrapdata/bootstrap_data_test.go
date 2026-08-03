package bootstrapdata

import "testing"

func TestNewCommand(t *testing.T) {
	t.Parallel()
	command := NewCommand()
	if command.Use != "fetch-bootstrap-data" {
		t.Fatalf("Use = %q", command.Use)
	}
	for _, name := range []string{
		"cluster-resource-id", "agent-pool-name", "auth", "msi-client-id",
		"sp-tenant-id", "sp-client-id", "sp-client-secret-file",
		"sp-client-certificate-file", "sp-client-credential-file",
		"resource-manager-endpoint", "authority-host", "api-version", "output",
	} {
		if command.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is missing", name)
		}
	}
}
