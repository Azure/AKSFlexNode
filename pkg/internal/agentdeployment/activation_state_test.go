package agentdeployment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// PATH is overridden to exercise the command boundary without host systemd.
func TestInspectAgentServiceActiveAfterUnitRemoval(t *testing.T) {
	dir := t.TempDir()
	command := filepath.Join(dir, "systemctl")
	const script = `#!/bin/sh
case "$*" in
  *--property=LoadState*) printf '%s\n' not-found ;;
  *--property=ActiveState*) printf '%s\n' "$FLEX_TEST_ACTIVE_STATE" ;;
  *) exit 1 ;;
esac
`
	if err := utilio.InstallFile(command, strings.NewReader(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	for _, tc := range []struct {
		name    string
		state   string
		active  bool
		wantErr bool
	}{
		{name: "removed unit still active", state: "active", active: true},
		{name: "removed unit inactive", state: "inactive"},
		{name: "failed service still needs recovery", state: "failed", active: true},
		{name: "unknown activity is not inactive", state: "unknown", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FLEX_TEST_ACTIVE_STATE", tc.state)
			active, err := inspectAgentServiceActive(t.Context(), discardLogger(), ServiceUnitName)
			if (err != nil) != tc.wantErr || active != tc.active {
				t.Fatalf("activity = %v, %v; want active=%v error=%v", active, err, tc.active, tc.wantErr)
			}
		})
	}
}
