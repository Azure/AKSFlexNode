package hostroot

import (
	"bytes"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
)

// TestHostRootCommand pins the output the install scripts and the AgentUpgrade
// downgrade guard read. The guard compares it with the root the running agent
// resolved, so anything but the bare path on one line refuses every upgrade.
func TestHostRootCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{name: "prints the planned root", args: nil, want: daemon.PlannedHostRoot() + "\n"},
		{name: "rejects arguments", args: []string{"extra"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			cmd := NewCommand()
			cmd.SetOut(&out)
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)

			err := cmd.Execute()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && out.String() != tt.want {
				t.Fatalf("output = %q, want %q", out.String(), tt.want)
			}
			if !cmd.Hidden {
				t.Fatal("host-root is for the agent's own tooling, not for operators")
			}
		})
	}
}
