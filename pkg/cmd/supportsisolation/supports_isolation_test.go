package supportsisolation

import "testing"

func TestProbeSupportsOCI(t *testing.T) {
	t.Parallel()
	cmd := NewCommand()
	cmd.SetArgs([]string{"container"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("OCI probe: %v", err)
	}
}
