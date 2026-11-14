package supportsisolation

import "testing"

func TestSupportedModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "none", args: []string{"none"}},
		{name: "nspawn", args: []string{"nspawn"}},
		{name: "container", args: []string{"container"}},
		{name: "unknown", args: []string{"invalid"}, wantErr: true},
		{name: "missing argument", wantErr: true},
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
