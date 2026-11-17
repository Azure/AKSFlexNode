package agentruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestNoneResetRemovesOnlyUpgradeArtifacts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, goalstates.AgentConfigDir)
	for _, name := range []string{"none-upgrade-config", "unrelated"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "credential"), []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal := filepath.Join(dir, "none-upgrade-journal.json")
	if err := os.WriteFile(journal, []byte("journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	mountInfo := filepath.Join(root, "mountinfo")
	if err := os.WriteFile(mountInfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	task := &noneResetTask{
		log: slog.New(slog.DiscardHandler),
		teardown: &noneops.Teardown{
			Root:       root,
			Layout:     none.Layout{Base: filepath.Join(root, "node")},
			SystemdDir: filepath.Join(root, "units"),
			MountInfo:  mountInfo,
			Proc:       t.TempDir(),
			Run: func(context.Context, *slog.Logger, string, ...string) (string, error) {
				return "", nil
			},
		},
	}
	for range 2 {
		if err := task.Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	// A later join can recreate the SDK layout and recovery files.
	if _, err := task.teardown.Layout.Ensure("rejoined"); err != nil {
		t.Fatal(err)
	}
	if _, err := task.teardown.Layout.Flip("rejoined"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "none-upgrade-config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("new journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := task.Do(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"none-upgrade-journal.json", "none-upgrade-config"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "unrelated", "credential")); err != nil {
		t.Fatalf("unrelated agent data was removed: %v", err)
	}
}

func TestNoneResetKeepsRecoveryArtifactsWhenStopFails(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	units := filepath.Join(root, "units")
	if err := os.MkdirAll(units, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, none.UnitKubelet), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, goalstates.AgentConfigDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(dir, "none-upgrade-journal.json")
	if err := os.WriteFile(journal, []byte("journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &noneResetTask{
		log: slog.New(slog.DiscardHandler),
		teardown: &noneops.Teardown{
			Root:       root,
			Layout:     none.Layout{Base: filepath.Join(root, "node")},
			SystemdDir: units,
			Run: func(context.Context, *slog.Logger, string, ...string) (string, error) {
				return "", errors.New("stop failed")
			},
		},
	}
	if err := task.Do(t.Context()); err == nil {
		t.Fatal("reset succeeded despite failed stop")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("recovery journal removed after failed teardown: %v", err)
	}
}
