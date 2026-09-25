package main

import "testing"

func TestRootCommandRegistersGeneratedNSpawnLifecycleShape(t *testing.T) {
	t.Parallel()

	cmd, remaining, err := newRootCommand().Find([]string{"nspawn-lifecycle", "pre-start", "kube1"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if cmd.Name() != "pre-start" {
		t.Fatalf("Find() command = %q, want pre-start", cmd.Name())
	}
	if len(remaining) != 1 || remaining[0] != "kube1" {
		t.Fatalf("Find() remaining args = %v, want [kube1]", remaining)
	}
}

// TestRootCommandRegistersTopLevelCommands is not parallel. newRootCommand
// attaches the package-level token.Command, so building two roots at once races
// on it. The subtests share one root for the same reason.
//
// host-root is what the install scripts and AgentUpgrade ask a release for, so
// a build that dropped it would be taken for a release before the host root.
func TestRootCommandRegistersTopLevelCommands(t *testing.T) {
	root := newRootCommand()

	for _, name := range []string{"ignition", "host-root"} {
		t.Run(name, func(t *testing.T) {
			cmd, _, err := root.Find([]string{name})
			if err != nil {
				t.Fatalf("Find() error = %v", err)
			}
			if cmd.Name() != name {
				t.Fatalf("Find() command = %q, want %s", cmd.Name(), name)
			}
		})
	}
}
