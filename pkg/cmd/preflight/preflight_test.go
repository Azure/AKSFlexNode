package preflight

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestNewCommand(t *testing.T) {
	t.Parallel()

	cmd := NewCommand()
	if cmd.Use != "preflight" {
		t.Fatalf("Use = %q, want preflight", cmd.Use)
	}

	for _, flag := range []string{"config", "ignore-preflight-errors", "fail-on-warnings", "output"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("expected flag %q", flag)
		}
	}
}

func TestWriteText(t *testing.T) {
	t.Parallel()

	report := preflight.Report{
		Checks: []preflight.Result{
			preflight.OK("ok-check", "ok target", "all good"),
			preflight.Warning("warn-check", "warn target", "be careful"),
			preflight.Error("error-check", "error target", "bad thing"),
		},
	}

	var out bytes.Buffer
	if err := writeText(&out, report); err != nil {
		t.Fatalf("writeText() error = %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"[preflight] Running AKS Flex Node preflight checks",
		"[OK ok-check]: all good (target: ok target)",
		"[WARNING warn-check]: be careful (target: warn target)",
		"[ERROR error-check]: bad thing (target: error target)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("writeText() output missing %q\n%s", want, got)
		}
	}
}
