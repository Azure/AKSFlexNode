package daemon

import "testing"

func TestNewCommands(t *testing.T) {
	t.Parallel()

	commands := NewCommands()
	if len(commands) != 3 {
		t.Fatalf("len(NewCommands()) = %d, want 3", len(commands))
	}
	want := map[string]bool{
		"daemon":                false,
		"agent-upgrade":         false,
		"recover-agent-upgrade": false,
	}
	for _, command := range commands {
		if _, ok := want[command.Name()]; !ok {
			t.Fatalf("unexpected command %q", command.Name())
		}
		if want[command.Name()] {
			t.Fatalf("duplicate command %q", command.Name())
		}
		want[command.Name()] = true
	}
	for name, found := range want {
		if !found {
			t.Errorf("command %q is missing", name)
		}
	}
}
