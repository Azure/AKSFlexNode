package daemon

import (
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
)

func testAgentUpgradePaths(t *testing.T) agentdeployment.UpgradePaths {
	t.Helper()
	dir := t.TempDir()
	return agentdeployment.UpgradePaths{
		BinaryPath:   filepath.Join(dir, "bin", "aks-flex-node"),
		BluePath:     filepath.Join(dir, "lib", "aks-flex-node-blue"),
		GreenPath:    filepath.Join(dir, "lib", "aks-flex-node-green"),
		CurrentPath:  filepath.Join(dir, "lib", "aks-flex-node-current"),
		LastGoodPath: filepath.Join(dir, "lib", "aks-flex-node-last-good"),
		SignalPath:   filepath.Join(dir, "etc", "agent-upgrade-signal.json"),
	}
}

func assertResolvedPath(t *testing.T, path, want string) {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", path, err)
	}
	if got != want {
		t.Fatalf("resolved %s = %s, want %s", path, got, want)
	}
}
