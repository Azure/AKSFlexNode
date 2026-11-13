package noneops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestApplicationArtifactVersionIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, version string
	}{
		{name: "legacy"},
		{name: "npd old", version: "v1.35.1"},
		{name: "npd new", version: "v1.36.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rf := &goalstates.RootFS{
				KubernetesVersion: "1.34.1", ContainerdVersion: "2.1.8",
				RunCVersion: "1.5.0", CNIPluginVersion: "1.5.1", HostArch: "amd64",
			}
			got := VersionFor(rf, tc.version)
			if tc.version == "" {
				if got != "1.34.1-ad4151aa" {
					t.Fatalf("legacy directory identity changed: %s", got)
				}
			} else if got == VersionFor(rf, "") || got == VersionFor(rf, tc.version+"-other") {
				t.Fatal("application binaries do not affect directory identity")
			}
			if got != VersionFor(rf, tc.version) {
				t.Fatal("unchanged artifacts changed directory identity")
			}
		})
	}
}

func TestNPDOnlyUpgradePreservesPreviousArtifacts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, previous, appliedGeneration, appliedArtifact string
		failReady                                          bool
	}{
		{name: "failed target rolls back", previous: "v1.35.1", failReady: true},
		{name: "interrupted before persistence", previous: "v1.35.1", appliedGeneration: "42", appliedArtifact: "v1.35.1"},
		{name: "artifact identity not persisted", previous: "v1.35.1", appliedGeneration: "43", appliedArtifact: "v1.35.1"},
		{name: "committed target", previous: "v1.35.1", appliedGeneration: "43", appliedArtifact: "v1.36.0"},
		{name: "legacy previous identity", appliedGeneration: "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newNodeFixture(t, "1.34.1")
			const configPath = "/etc/node-problem-detector/kernel-monitor.json"
			install := func(content string) func(context.Context, *goalstates.MachineGoalState) error {
				return func(_ context.Context, gs *goalstates.MachineGoalState) error {
					mustWrite(t, gs.RootFS.Root().Path("/usr/local/bin/node-problem-detector"), content)
					mustWrite(t, gs.RootFS.Root().Path(configPath), content)
					return nil
				}
			}
			f.node.p.ArtifactVersion = tc.previous
			f.node.p.Install = install("old detector")
			if err := f.node.Provision(t.Context()); err != nil {
				t.Fatal(err)
			}
			oldVersion, err := f.node.Version()
			if err != nil {
				t.Fatal(err)
			}
			oldBinary := f.node.gs.RootFS.Root().Path("/usr/local/bin/node-problem-detector")

			p := f.node.p
			p.Generation, p.ArtifactVersion = "43", "v1.36.0"
			p.Previous, p.PreviousGoalState = p.Config, p.GoalState
			p.PreviousArtifactVersion = tc.previous
			p.GoalState = newNodeFixture(t, "1.34.1").node.p.GoalState
			p.Install = install("new detector")
			var selectedTarget string
			if tc.failReady {
				p.SetupGPU = func(context.Context) error {
					current, err := p.Layout.Current()
					if err != nil {
						return err
					}
					if current == selectedTarget {
						return errors.New("target containerd preparation failed")
					}
					return nil
				}
			}
			target := New(p)
			targetVersion, err := target.Version()
			if err != nil {
				t.Fatal(err)
			}
			selectedTarget = targetVersion
			if targetVersion == oldVersion {
				t.Fatal("NPD-only upgrade reused the active directory")
			}
			previous, err := target.previous()
			if err != nil {
				t.Fatal(err)
			}
			if previous.version != oldVersion {
				t.Fatalf("previous directory = %s, want %s", previous.version, oldVersion)
			}
			targetBinary := target.gs.RootFS.Root().Path("/usr/local/bin/node-problem-detector")
			err = target.Upgrade(t.Context())
			if (err != nil) != tc.failReady {
				t.Fatalf("Upgrade error = %v", err)
			}
			j, err := target.readJournal()
			if err != nil {
				t.Fatal(err)
			}
			if j.TargetArtifactVersion != "v1.36.0" {
				t.Fatal("journal did not record the target application artifact identity")
			}
			if !tc.failReady {
				p.Generation, p.ArtifactVersion = tc.appliedGeneration, tc.appliedArtifact
				if err := New(p).RecoverUpgrade(t.Context()); err != nil {
					t.Fatal(err)
				}
			}

			wantVersion, wantConfig := oldVersion, "old detector"
			if !tc.failReady {
				// Once exposed, an unpersisted target completes forward rather
				// than reverting binaries underneath its new workloads.
				wantVersion, wantConfig = targetVersion, "new detector"
			}
			current, err := p.Layout.Current()
			if err != nil || current != wantVersion {
				t.Fatalf("current directory = %s, want %s; error = %v", current, wantVersion, err)
			}
			for path, want := range map[string]string{
				oldBinary: "old detector", targetBinary: "new detector",
				filepath.Join(f.root, configPath): wantConfig,
			} {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != want {
					t.Fatalf("artifact %s = %q, want %q; error = %v", path, data, want, err)
				}
			}
		})
	}
}
