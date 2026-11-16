package supportsisolation

import (
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestSupportedModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "none", args: []string{string(isolation.ModeNone)}},
		{name: "nspawn", args: []string{string(isolation.ModeNSpawn)}},
		{name: "container", args: []string{string(isolation.ModeContainer)}},
		{name: "unknown", args: []string{"invalid"}, wantErr: true},
		{name: "empty argument", args: []string{""}, wantErr: true},
		{name: "whitespace", args: []string{" "}, wantErr: true},
		{name: "wrong case", args: []string{"NSPAWN"}, wantErr: true},
		{name: "missing argument", wantErr: true},
		{name: "extra argument", args: []string{string(isolation.ModeNSpawn), "extra"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := NewCommand()
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); (err != nil) != tc.wantErr {
				t.Fatalf("Execute = %v", err)
			}
		})
	}
}
