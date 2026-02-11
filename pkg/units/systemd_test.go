package units

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGenerateDeltas_BothEmpty(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if len(deltas.UnitsToStart) != 0 {
		t.Errorf("UnitsToStart = %v, want empty", deltas.UnitsToStart)
	}
	if len(deltas.UnitsToStop) != 0 {
		t.Errorf("UnitsToStop = %v, want empty", deltas.UnitsToStop)
	}
	if len(deltas.UnitsToRestart) != 0 {
		t.Errorf("UnitsToRestart = %v, want empty", deltas.UnitsToRestart)
	}
	if len(deltas.UnitsToReload) != 0 {
		t.Errorf("UnitsToReload = %v, want empty", deltas.UnitsToReload)
	}
}

func TestGenerateDeltas_EmptyOldPath(t *testing.T) {
	// Empty old path means no previous generation — everything is new.
	newDir := t.TempDir()
	os.WriteFile(filepath.Join(newDir, "foo.service"), []byte("[Service]\nExecStart=/bin/foo"), 0644)
	os.WriteFile(filepath.Join(newDir, "bar.service"), []byte("[Service]\nExecStart=/bin/bar"), 0644)

	deltas, err := generateDeltas("", newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	want := []string{"bar.service", "foo.service"}
	if !reflect.DeepEqual(deltas.UnitsToStart, want) {
		t.Errorf("UnitsToStart = %v, want %v", deltas.UnitsToStart, want)
	}
	if len(deltas.UnitsToStop) != 0 {
		t.Errorf("UnitsToStop = %v, want empty", deltas.UnitsToStop)
	}
	if len(deltas.UnitsToRestart) != 0 {
		t.Errorf("UnitsToRestart = %v, want empty", deltas.UnitsToRestart)
	}
}

func TestGenerateDeltas_EmptyNewPath(t *testing.T) {
	// Empty new path means nothing in the new generation — everything stops.
	oldDir := t.TempDir()
	os.WriteFile(filepath.Join(oldDir, "foo.service"), []byte("[Service]\nExecStart=/bin/foo"), 0644)

	deltas, err := generateDeltas(oldDir, "")
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	want := []string{"foo.service"}
	if !reflect.DeepEqual(deltas.UnitsToStop, want) {
		t.Errorf("UnitsToStop = %v, want %v", deltas.UnitsToStop, want)
	}
	if len(deltas.UnitsToStart) != 0 {
		t.Errorf("UnitsToStart = %v, want empty", deltas.UnitsToStart)
	}
}

func TestGenerateDeltas_NewUnits(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	// Old has one unit, new has two (one added).
	os.WriteFile(filepath.Join(oldDir, "existing.service"), []byte("same"), 0644)
	os.WriteFile(filepath.Join(newDir, "existing.service"), []byte("same"), 0644)
	os.WriteFile(filepath.Join(newDir, "added.service"), []byte("new"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStart, []string{"added.service"}) {
		t.Errorf("UnitsToStart = %v, want [added.service]", deltas.UnitsToStart)
	}
	if len(deltas.UnitsToStop) != 0 {
		t.Errorf("UnitsToStop = %v, want empty", deltas.UnitsToStop)
	}
	if len(deltas.UnitsToRestart) != 0 {
		t.Errorf("UnitsToRestart = %v, want empty", deltas.UnitsToRestart)
	}
}

func TestGenerateDeltas_RemovedUnits(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	os.WriteFile(filepath.Join(oldDir, "existing.service"), []byte("same"), 0644)
	os.WriteFile(filepath.Join(oldDir, "removed.service"), []byte("gone"), 0644)
	os.WriteFile(filepath.Join(newDir, "existing.service"), []byte("same"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStop, []string{"removed.service"}) {
		t.Errorf("UnitsToStop = %v, want [removed.service]", deltas.UnitsToStop)
	}
	if len(deltas.UnitsToStart) != 0 {
		t.Errorf("UnitsToStart = %v, want empty", deltas.UnitsToStart)
	}
}

func TestGenerateDeltas_ChangedUnits(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	os.WriteFile(filepath.Join(oldDir, "app.service"), []byte("[Service]\nExecStart=/bin/app-v1"), 0644)
	os.WriteFile(filepath.Join(newDir, "app.service"), []byte("[Service]\nExecStart=/bin/app-v2"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToRestart, []string{"app.service"}) {
		t.Errorf("UnitsToRestart = %v, want [app.service]", deltas.UnitsToRestart)
	}
	if len(deltas.UnitsToStart) != 0 {
		t.Errorf("UnitsToStart = %v, want empty", deltas.UnitsToStart)
	}
	if len(deltas.UnitsToStop) != 0 {
		t.Errorf("UnitsToStop = %v, want empty", deltas.UnitsToStop)
	}
}

func TestGenerateDeltas_UnchangedUnits(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	content := []byte("[Service]\nExecStart=/bin/app")
	os.WriteFile(filepath.Join(oldDir, "app.service"), content, 0644)
	os.WriteFile(filepath.Join(newDir, "app.service"), content, 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if len(deltas.UnitsToStart) != 0 || len(deltas.UnitsToStop) != 0 ||
		len(deltas.UnitsToRestart) != 0 || len(deltas.UnitsToReload) != 0 {
		t.Errorf("expected no deltas for unchanged units, got start=%v stop=%v restart=%v reload=%v",
			deltas.UnitsToStart, deltas.UnitsToStop, deltas.UnitsToRestart, deltas.UnitsToReload)
	}
}

func TestGenerateDeltas_MixedOperations(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	// unchanged
	os.WriteFile(filepath.Join(oldDir, "stable.service"), []byte("[Service]\nExecStart=/bin/stable"), 0644)
	os.WriteFile(filepath.Join(newDir, "stable.service"), []byte("[Service]\nExecStart=/bin/stable"), 0644)

	// changed
	os.WriteFile(filepath.Join(oldDir, "changed.service"), []byte("[Service]\nExecStart=/bin/old"), 0644)
	os.WriteFile(filepath.Join(newDir, "changed.service"), []byte("[Service]\nExecStart=/bin/new"), 0644)

	// removed
	os.WriteFile(filepath.Join(oldDir, "removed.service"), []byte("[Service]\nExecStart=/bin/bye"), 0644)

	// added
	os.WriteFile(filepath.Join(newDir, "added.service"), []byte("[Service]\nExecStart=/bin/hello"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStart, []string{"added.service"}) {
		t.Errorf("UnitsToStart = %v, want [added.service]", deltas.UnitsToStart)
	}
	if !reflect.DeepEqual(deltas.UnitsToStop, []string{"removed.service"}) {
		t.Errorf("UnitsToStop = %v, want [removed.service]", deltas.UnitsToStop)
	}
	if !reflect.DeepEqual(deltas.UnitsToRestart, []string{"changed.service"}) {
		t.Errorf("UnitsToRestart = %v, want [changed.service]", deltas.UnitsToRestart)
	}
}

func TestGenerateDeltas_IgnoresNonUnitFiles(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	os.WriteFile(filepath.Join(oldDir, "README.md"), []byte("docs"), 0644)
	os.WriteFile(filepath.Join(oldDir, "config.toml"), []byte("cfg"), 0644)
	os.WriteFile(filepath.Join(newDir, "notes.txt"), []byte("notes"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if len(deltas.UnitsToStart) != 0 || len(deltas.UnitsToStop) != 0 {
		t.Errorf("expected no deltas for non-unit files, got start=%v stop=%v",
			deltas.UnitsToStart, deltas.UnitsToStop)
	}
}

func TestGenerateDeltas_IgnoresSubdirectories(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	// Create a subdirectory with a unit file — should be ignored.
	os.MkdirAll(filepath.Join(oldDir, "subdir"), 0755)
	os.WriteFile(filepath.Join(oldDir, "subdir", "nested.service"), []byte("unit"), 0644)
	os.WriteFile(filepath.Join(newDir, "real.service"), []byte("unit"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStart, []string{"real.service"}) {
		t.Errorf("UnitsToStart = %v, want [real.service]", deltas.UnitsToStart)
	}
	if len(deltas.UnitsToStop) != 0 {
		t.Errorf("UnitsToStop = %v, want empty (subdirs should be ignored)", deltas.UnitsToStop)
	}
}

func TestGenerateDeltas_NonexistentOldDir(t *testing.T) {
	newDir := t.TempDir()
	os.WriteFile(filepath.Join(newDir, "app.service"), []byte("unit"), 0644)

	deltas, err := generateDeltas("/nonexistent/dir", newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStart, []string{"app.service"}) {
		t.Errorf("UnitsToStart = %v, want [app.service]", deltas.UnitsToStart)
	}
}

func TestGenerateDeltas_NonexistentNewDir(t *testing.T) {
	oldDir := t.TempDir()
	os.WriteFile(filepath.Join(oldDir, "app.service"), []byte("unit"), 0644)

	deltas, err := generateDeltas(oldDir, "/nonexistent/dir")
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStop, []string{"app.service"}) {
		t.Errorf("UnitsToStop = %v, want [app.service]", deltas.UnitsToStop)
	}
}

func TestGenerateDeltas_MultipleUnitTypes(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	os.WriteFile(filepath.Join(newDir, "foo.service"), []byte("svc"), 0644)
	os.WriteFile(filepath.Join(newDir, "foo.socket"), []byte("sock"), 0644)
	os.WriteFile(filepath.Join(newDir, "foo.timer"), []byte("timer"), 0644)
	os.WriteFile(filepath.Join(newDir, "bar.mount"), []byte("mount"), 0644)
	os.WriteFile(filepath.Join(newDir, "baz.target"), []byte("target"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	want := []string{"bar.mount", "baz.target", "foo.service", "foo.socket", "foo.timer"}
	if !reflect.DeepEqual(deltas.UnitsToStart, want) {
		t.Errorf("UnitsToStart = %v, want %v", deltas.UnitsToStart, want)
	}
}

func TestGenerateDeltas_SortedOutput(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	// Create units in non-alphabetical order.
	os.WriteFile(filepath.Join(newDir, "zebra.service"), []byte("z"), 0644)
	os.WriteFile(filepath.Join(newDir, "alpha.service"), []byte("a"), 0644)
	os.WriteFile(filepath.Join(newDir, "middle.service"), []byte("m"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	want := []string{"alpha.service", "middle.service", "zebra.service"}
	if !reflect.DeepEqual(deltas.UnitsToStart, want) {
		t.Errorf("UnitsToStart = %v, want sorted %v", deltas.UnitsToStart, want)
	}
}

func TestIsSystemdUnitFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"foo.service", true},
		{"foo.socket", true},
		{"foo.timer", true},
		{"foo.mount", true},
		{"foo.automount", true},
		{"foo.swap", true},
		{"foo.target", true},
		{"foo.path", true},
		{"foo.slice", true},
		{"foo.scope", true},
		{"foo.device", true},
		{"foo.txt", false},
		{"foo.conf", false},
		{"README.md", false},
		{".service", true}, // edge case, but suffix matches
		{"service", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSystemdUnitFile(tt.name)
			if got != tt.want {
				t.Errorf("isSystemdUnitFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestFakeSystemdManager_RecordsActions(t *testing.T) {
	mgr := &FakeManager{}

	mgr.StartUnit("foo.service")
	mgr.RestartUnit("bar.service")
	mgr.ReloadUnit("baz.service")
	mgr.StopUnit("old.service")
	mgr.ReloadDaemon()

	want := []string{
		"start:foo.service",
		"restart:bar.service",
		"reload:baz.service",
		"stop:old.service",
		"daemon-reload",
	}

	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}

func TestApplyDeltas_ExecutesInOrder(t *testing.T) {
	mgr := &FakeManager{}

	deltas := &systemdUnitDeltas{
		UnitsToStop:    []string{"old.service"},
		UnitsToStart:   []string{"new.service"},
		UnitsToRestart: []string{"changed.service"},
		UnitsToReload:  []string{"reload-me.service"},
	}

	if err := applyDeltas(mgr, deltas); err != nil {
		t.Fatalf("applyDeltas() error = %v", err)
	}

	want := []string{
		"stop:old.service",
		"daemon-reload",
		"start:new.service",
		"restart:changed.service",
		"reload:reload-me.service",
	}

	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}

func TestApplyDeltas_EmptyDeltas(t *testing.T) {
	mgr := &FakeManager{}

	deltas := &systemdUnitDeltas{}

	if err := applyDeltas(mgr, deltas); err != nil {
		t.Fatalf("applyDeltas() error = %v", err)
	}

	// Should still daemon-reload even with no units.
	want := []string{"daemon-reload"}
	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}

func TestApplyDeltas_MultipleUnitsPerPhase(t *testing.T) {
	mgr := &FakeManager{}

	deltas := &systemdUnitDeltas{
		UnitsToStop:    []string{"a.service", "b.service"},
		UnitsToStart:   []string{"c.service", "d.service"},
		UnitsToRestart: []string{"e.service", "f.service"},
	}

	if err := applyDeltas(mgr, deltas); err != nil {
		t.Fatalf("applyDeltas() error = %v", err)
	}

	want := []string{
		"stop:a.service",
		"stop:b.service",
		"daemon-reload",
		"start:c.service",
		"start:d.service",
		"restart:e.service",
		"restart:f.service",
	}

	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}

func TestResolveUnitDir(t *testing.T) {
	pkg := &InstalledPackage{
		InstalledStatePath: "/aks-flex/states/etc-overlay-abc123",
	}

	got := resolveUnitDir(pkg)
	want := "/aks-flex/states/etc-overlay-abc123/etc/systemd/system"
	if got != want {
		t.Errorf("resolveUnitDir() = %q, want %q", got, want)
	}
}

func TestResolveUnitDir_Nil(t *testing.T) {
	got := resolveUnitDir(nil)
	if got != "" {
		t.Errorf("resolveUnitDir(nil) = %q, want empty", got)
	}
}

// TestGenerateDeltas_EndToEnd_TwoGenerations simulates two overlay generations
// and verifies that generateDeltas correctly identifies what to start, stop,
// and restart when switching from gen1 to gen2.
func TestGenerateDeltas_EndToEnd_TwoGenerations(t *testing.T) {
	gen1Dir := t.TempDir()
	gen2Dir := t.TempDir()

	// Gen 1: containerd + kubelet
	os.WriteFile(filepath.Join(gen1Dir, "containerd.service"), []byte("[Service]\nExecStart=/v1/bin/containerd"), 0644)
	os.WriteFile(filepath.Join(gen1Dir, "kubelet.service"), []byte("[Service]\nExecStart=/v1/bin/kubelet"), 0644)

	// Gen 2: containerd (changed) + new calico, kubelet removed
	os.WriteFile(filepath.Join(gen2Dir, "containerd.service"), []byte("[Service]\nExecStart=/v2/bin/containerd\nRestart=always"), 0644)
	os.WriteFile(filepath.Join(gen2Dir, "calico-node.service"), []byte("[Service]\nExecStart=/v2/bin/calico-node"), 0644)

	deltas, err := generateDeltas(gen1Dir, gen2Dir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if !reflect.DeepEqual(deltas.UnitsToStart, []string{"calico-node.service"}) {
		t.Errorf("UnitsToStart = %v, want [calico-node.service]", deltas.UnitsToStart)
	}
	if !reflect.DeepEqual(deltas.UnitsToStop, []string{"kubelet.service"}) {
		t.Errorf("UnitsToStop = %v, want [kubelet.service]", deltas.UnitsToStop)
	}
	if !reflect.DeepEqual(deltas.UnitsToRestart, []string{"containerd.service"}) {
		t.Errorf("UnitsToRestart = %v, want [containerd.service]", deltas.UnitsToRestart)
	}

	// Now apply the deltas to a fake manager and verify order.
	mgr := &FakeManager{}
	if err := applyDeltas(mgr, deltas); err != nil {
		t.Fatalf("applyDeltas() error = %v", err)
	}

	want := []string{
		"stop:kubelet.service",
		"daemon-reload",
		"start:calico-node.service",
		"restart:containerd.service",
	}
	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}

func TestWalkUnitDir_WithSymlinks(t *testing.T) {
	// Simulate the etc overlay structure: the unit dir contains symlinks
	// to actual files in package state dirs.
	realDir := filepath.Join(t.TempDir(), "real-pkg")
	os.MkdirAll(realDir, 0755)
	os.WriteFile(filepath.Join(realDir, "containerd.service"), []byte("[Service]\nExecStart=/bin/containerd"), 0644)

	unitDir := filepath.Join(t.TempDir(), "etc", "systemd", "system")
	os.MkdirAll(unitDir, 0755)
	os.Symlink(filepath.Join(realDir, "containerd.service"), filepath.Join(unitDir, "containerd.service"))

	units, err := walkUnitDir(unitDir)
	if err != nil {
		t.Fatalf("walkUnitDir() error = %v", err)
	}

	if len(units) != 1 {
		t.Fatalf("expected 1 unit, got %d", len(units))
	}

	content, ok := units["containerd.service"]
	if !ok {
		t.Fatal("expected containerd.service in result")
	}
	if string(content) != "[Service]\nExecStart=/bin/containerd" {
		t.Errorf("content = %q, want [Service]\\nExecStart=/bin/containerd", content)
	}
}

func TestWalkUnitDir_EmptyPath(t *testing.T) {
	units, err := walkUnitDir("")
	if err != nil {
		t.Fatalf("walkUnitDir('') error = %v", err)
	}
	if len(units) != 0 {
		t.Errorf("expected empty map, got %v", units)
	}
}

func TestWalkUnitDir_NonexistentPath(t *testing.T) {
	units, err := walkUnitDir("/nonexistent/path")
	if err != nil {
		t.Fatalf("walkUnitDir() error = %v", err)
	}
	if len(units) != 0 {
		t.Errorf("expected empty map, got %v", units)
	}
}

func TestFakeManager_ImplementsInterface(t *testing.T) {
	// Compile-time check that FakeManager implements Manager.
	var _ Manager = (*FakeManager)(nil)
}

func TestFakeManager_NoErrorsReturned(t *testing.T) {
	mgr := &FakeManager{}

	if err := mgr.StartUnit("test.service"); err != nil {
		t.Errorf("StartUnit() error = %v", err)
	}
	if err := mgr.StopUnit("test.service"); err != nil {
		t.Errorf("StopUnit() error = %v", err)
	}
	if err := mgr.RestartUnit("test.service"); err != nil {
		t.Errorf("RestartUnit() error = %v", err)
	}
	if err := mgr.ReloadUnit("test.service"); err != nil {
		t.Errorf("ReloadUnit() error = %v", err)
	}
	if err := mgr.ReloadDaemon(); err != nil {
		t.Errorf("ReloadDaemon() error = %v", err)
	}
}

func TestFakeManager_ActionsInitiallyEmpty(t *testing.T) {
	mgr := &FakeManager{}

	if len(mgr.Actions) != 0 {
		t.Errorf("Actions = %v, want empty", mgr.Actions)
	}
}

func TestFakeManager_MultipleUnitsOfSameType(t *testing.T) {
	mgr := &FakeManager{}

	mgr.StartUnit("a.service")
	mgr.StartUnit("b.service")
	mgr.StartUnit("c.service")

	want := []string{
		"start:a.service",
		"start:b.service",
		"start:c.service",
	}

	if !reflect.DeepEqual(mgr.Actions, want) {
		t.Errorf("Actions = %v, want %v", mgr.Actions, want)
	}
}
