package nodelifecycle

import (
	"strings"
	"testing"
)

func TestCommandRejectsInvalidHooks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "missing slot", args: []string{"pre-start"}, want: "accepts 1 arg"},
		{name: "invalid slot", args: []string{"post-start", "kube3"}, want: "invalid container slot"},
		{name: "unknown phase", args: []string{"unknown", "kube1"}, want: "unknown command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := NewCommand()
			cmd.SilenceErrors = true
			cmd.SilenceUsage = true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
