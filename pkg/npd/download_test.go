package npd

import (
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

func TestPreflight(t *testing.T) {
	t.Parallel()

	checks := Preflight(&config.Config{Npd: config.NPDConfig{Version: "v1.2.3"}})
	if len(checks) != 1 {
		t.Fatalf("Preflight() returned %d checks, want 1", len(checks))
	}
	if got := checks[0].Name(); got != npdArtifactCheckName {
		t.Fatalf("Preflight()[0].Name() = %q, want %q", got, npdArtifactCheckName)
	}
}
