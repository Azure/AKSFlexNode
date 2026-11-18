package agentdeployment

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestContainerConvergenceWritesUnitsBeforeReloadAndImageCleanup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	unitDir := filepath.Join(dir, "systemd")
	var events []string
	convergence := &ContainerConvergence{
		log:        discardLogger(),
		unitDir:    unitDir,
		signalPath: filepath.Join(dir, "signal.json"),
		images:     agentImageEnvFile{path: filepath.Join(dir, "images.env")},
		install: agentImageEnvFile{path: writeTestFile(t, filepath.Join(dir, "settings"),
			"AKS_FLEX_NODE_IMAGE="+testAgentImageV1+"\n")},
		runningImage: testAgentImageV1,
		reload: func(context.Context) error {
			if _, err := os.Stat(filepath.Join(unitDir, ServiceUnitName)); err != nil {
				t.Fatalf("agent unit missing at reload: %v", err)
			}
			events = append(events, "reload")
			return nil
		},
		listImages: func(context.Context) (string, error) {
			events = append(events, "list images")
			return testAgentImageV1, nil
		},
		ctr: func(context.Context, ...string) error {
			t.Error("removed an image in use")
			return nil
		},
	}
	held, err := convergence.Converge(t.Context())
	if err != nil || held {
		t.Fatalf("Converge = %v, %v; want false, nil", held, err)
	}
	if !slices.Equal(events, []string{"reload", "list images"}) {
		t.Fatalf("events = %v, want reload then image listing", events)
	}
}

func TestContainerConvergenceMissingWorkerSkipsExecutableResolution(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing", "bin", "aks-flex-node")
	convergence := &ContainerConvergence{
		nspawnBinaryPath: func(string) string { return missing },
		runningExecutable: func() (string, error) {
			t.Error("resolved executable for absent worker")
			return "", nil
		},
	}
	if err := convergence.SyncActiveWorker("kube1"); err != nil {
		t.Fatalf("synchronize absent worker: %v", err)
	}
}
