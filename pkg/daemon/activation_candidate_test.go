package daemon

import (
	"os"
	"testing"
)

// The running test binary is immutable, avoiding write/exec races on candidate
// scripts while keeping the real compatibility-probe process boundary.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "supports-isolation":
			if len(os.Args) == 3 && (os.Args[2] == "none" || os.Args[2] == "container") {
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func linkActivationCandidate(t *testing.T, path string) {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, path); err != nil {
		t.Fatal(err)
	}
}
