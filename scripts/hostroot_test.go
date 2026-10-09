package scripts

import (
	"os"
	"regexp"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

// TestScriptsInstallUnderTheAgentHostRoot keeps the scripts' HOST_ROOT in step
// with the agent library. The scripts choose where the binary goes and what
// uninstall removes, and the agent resolves every path from hostroot.Path; if
// the two disagree, a host is installed where the agent never looks, or
// uninstall removes a directory that is not the agent's.
func TestScriptsInstallUnderTheAgentHostRoot(t *testing.T) {
	t.Parallel()

	hostRoot := regexp.MustCompile(`(?m)^(?:readonly )?HOST_ROOT="([^"]*)"$`)

	for _, tt := range []struct {
		name   string
		script func(t *testing.T) string
	}{
		{name: "bootstrap.sh", script: func(*testing.T) string { return Bootstrap }},
		{name: "install.sh", script: readScript("install.sh")},
		{name: "uninstall.sh", script: readScript("uninstall.sh")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			matches := hostRoot.FindAllStringSubmatch(tt.script(t), -1)
			if len(matches) != 1 {
				t.Fatalf("want exactly one HOST_ROOT assignment, got %d", len(matches))
			}

			if got := matches[0][1]; got != hostroot.Path {
				t.Errorf("HOST_ROOT = %q, want hostroot.Path %q", got, hostroot.Path)
			}
		})
	}
}

func readScript(name string) func(t *testing.T) string {
	return func(t *testing.T) string {
		t.Helper()

		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}

		return string(data)
	}
}
