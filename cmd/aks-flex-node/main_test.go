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

func TestRootCommandRegistersIgnition(t *testing.T) {
	t.Parallel()

	cmd, _, err := newRootCommand().Find([]string{"ignition"})
	if err != nil {
		t.Fatalf("Find() error = %v", err)
	}
	if cmd.Name() != "ignition" {
		t.Fatalf("Find() command = %q, want ignition", cmd.Name())
	}
}
