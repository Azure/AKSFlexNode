package units

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// ---------------------------------------------------------------------------
// parseSystemdINI tests
// ---------------------------------------------------------------------------

func TestParseSystemdINI_BasicSections(t *testing.T) {
	content := []byte(`[Unit]
Description=My Service
After=network.target

[Service]
ExecStart=/usr/bin/myapp
Restart=always

[Install]
WantedBy=multi-user.target
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	// [Install] should be skipped.
	if _, ok := info["Install"]; ok {
		t.Error("expected [Install] section to be skipped, but it was parsed")
	}

	// [Unit] should be present.
	unitSection, ok := info["Unit"]
	if !ok {
		t.Fatal("expected [Unit] section to exist")
	}
	if desc := unitSection["Description"]; len(desc) != 1 || desc[0] != "My Service" {
		t.Errorf("Description = %v, want [My Service]", desc)
	}

	// [Service] should be present.
	svcSection, ok := info["Service"]
	if !ok {
		t.Fatal("expected [Service] section to exist")
	}
	if exec := svcSection["ExecStart"]; len(exec) != 1 || exec[0] != "/usr/bin/myapp" {
		t.Errorf("ExecStart = %v, want [/usr/bin/myapp]", exec)
	}
}

func TestParseSystemdINI_Comments(t *testing.T) {
	content := []byte(`# This is a comment
; This is also a comment
[Service]
# Another comment
ExecStart=/bin/foo
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	svc := info["Service"]
	if exec := svc["ExecStart"]; len(exec) != 1 || exec[0] != "/bin/foo" {
		t.Errorf("ExecStart = %v, want [/bin/foo]", exec)
	}
}

func TestParseSystemdINI_MultiValuedKeys(t *testing.T) {
	content := []byte(`[Service]
ExecStartPre=/bin/prep1
ExecStartPre=/bin/prep2
ExecStart=/bin/main
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	pre := info["Service"]["ExecStartPre"]
	want := []string{"/bin/prep1", "/bin/prep2"}
	if !reflect.DeepEqual(pre, want) {
		t.Errorf("ExecStartPre = %v, want %v", pre, want)
	}
}

func TestParseSystemdINI_EmptyValueClears(t *testing.T) {
	// Simulate an override where ExecStart= clears previous values.
	info := make(unitInfo)

	base := []byte(`[Service]
ExecStart=/bin/old
`)
	override := []byte(`[Service]
ExecStart=
ExecStart=/bin/new
`)

	if err := parseSystemdINI(info, base); err != nil {
		t.Fatalf("parseSystemdINI(base) error = %v", err)
	}
	if err := parseSystemdINI(info, override); err != nil {
		t.Fatalf("parseSystemdINI(override) error = %v", err)
	}

	exec := info["Service"]["ExecStart"]
	want := []string{"/bin/new"}
	if !reflect.DeepEqual(exec, want) {
		t.Errorf("ExecStart = %v, want %v (empty value should clear)", exec, want)
	}
}

func TestParseSystemdINI_EmptyValueClearsToNil(t *testing.T) {
	// ExecStart= alone should result in nil/empty slice.
	content := []byte(`[Service]
ExecStart=/bin/old
ExecStart=
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	exec := info["Service"]["ExecStart"]
	if len(exec) != 0 {
		t.Errorf("ExecStart = %v, want empty (cleared by empty value)", exec)
	}
}

func TestParseSystemdINI_PreservesEscapes(t *testing.T) {
	// Escape sequences like \x2d should be preserved literally.
	content := []byte(`[Unit]
Description=dev-disk-by\x2dlabel-root
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	desc := info["Unit"]["Description"]
	want := []string{`dev-disk-by\x2dlabel-root`}
	if !reflect.DeepEqual(desc, want) {
		t.Errorf("Description = %v, want %v", desc, want)
	}
}

func TestParseSystemdINI_SkipsLinesOutsideSection(t *testing.T) {
	content := []byte(`Key=Value
[Service]
ExecStart=/bin/foo
`)
	info, err := parseUnitContent(content)
	if err != nil {
		t.Fatalf("parseUnitContent() error = %v", err)
	}

	// Key=Value outside any section should be ignored.
	if len(info) != 1 {
		t.Errorf("expected 1 section, got %d", len(info))
	}
	if _, ok := info["Service"]; !ok {
		t.Error("expected [Service] section")
	}
}

// ---------------------------------------------------------------------------
// compareUnits tests
// ---------------------------------------------------------------------------

func TestCompareUnits_Equal(t *testing.T) {
	a := []byte(`[Unit]
Description=Test
[Service]
ExecStart=/bin/foo
Restart=always
`)
	b := []byte(`[Unit]
Description=Test
[Service]
ExecStart=/bin/foo
Restart=always
`)
	got, err := compareUnitContents(a, b)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual", got)
	}
}

func TestCompareUnits_ServiceChanged_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo-v1
`)
	new := []byte(`[Service]
ExecStart=/bin/foo-v2
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart", got)
	}
}

func TestCompareUnits_InstallSectionIgnored(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
[Install]
WantedBy=multi-user.target
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
[Install]
WantedBy=graphical.target
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (Install section should be ignored)", got)
	}
}

func TestCompareUnits_InstallSectionAdded(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
[Install]
WantedBy=multi-user.target
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (Install section should be ignored)", got)
	}
}

func TestCompareUnits_InstallSectionRemoved(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
[Install]
WantedBy=multi-user.target
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (Install section should be ignored)", got)
	}
}

func TestCompareUnits_DescriptionIgnored(t *testing.T) {
	old := []byte(`[Unit]
Description=Old Name
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Description=New Name
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (Description is ignored)", got)
	}
}

func TestCompareUnits_DocumentationIgnored(t *testing.T) {
	old := []byte(`[Unit]
Documentation=man:foo(1)
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Documentation=https://example.com/foo
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (Documentation is ignored)", got)
	}
}

func TestCompareUnits_AllIgnoredUnitKeys(t *testing.T) {
	// Test all keys in the unit section ignore list.
	ignoredKeys := []string{
		"Description", "Documentation", "OnFailure", "OnSuccess",
		"OnFailureJobMode", "IgnoreOnIsolate", "StopWhenUnneeded",
		"RefuseManualStart", "RefuseManualStop", "AllowIsolate",
		"CollectMode", "SourcePath",
	}

	for _, key := range ignoredKeys {
		t.Run(key, func(t *testing.T) {
			old := []byte("[Unit]\n" + key + "=old-value\n[Service]\nExecStart=/bin/foo\n")
			new := []byte("[Unit]\n" + key + "=new-value\n[Service]\nExecStart=/bin/foo\n")
			got, err := compareUnitContents(old, new)
			if err != nil {
				t.Fatalf("compareUnitContents() error = %v", err)
			}
			if got != UnitEqual {
				t.Errorf("compareUnitContents = %v, want UnitEqual (%s should be ignored)", got, key)
			}
		})
	}
}

func TestCompareUnits_UnitKeyAdded_Ignored(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Description=Added
Documentation=https://example.com
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (new [Unit] with only ignored keys)", got)
	}
}

func TestCompareUnits_UnitKeyRemoved_Ignored(t *testing.T) {
	old := []byte(`[Unit]
Description=Old
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (removed [Unit] had only ignored keys)", got)
	}
}

func TestCompareUnits_UnitNonIgnoredKey_NeedsRestart(t *testing.T) {
	old := []byte(`[Unit]
Requires=network.target
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Requires=other.target
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (Requires is not ignored)", got)
	}
}

func TestCompareUnits_UnitSectionRemovedWithNonIgnored_NeedsRestart(t *testing.T) {
	old := []byte(`[Unit]
Requires=network.target
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (removed [Unit] had non-ignored keys)", got)
	}
}

func TestCompareUnits_MountOptions_NeedsReload(t *testing.T) {
	old := []byte(`[Mount]
What=/dev/sda1
Where=/mnt/data
Options=defaults
`)
	new := []byte(`[Mount]
What=/dev/sda1
Where=/mnt/data
Options=defaults,noatime
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsReload {
		t.Errorf("compareUnitContents = %v, want UnitNeedsReload (Mount Options change)", got)
	}
}

func TestCompareUnits_MountDevice_NeedsRestart(t *testing.T) {
	old := []byte(`[Mount]
What=/dev/sda1
Where=/mnt/data
Options=defaults
`)
	new := []byte(`[Mount]
What=/dev/sdb1
Where=/mnt/data
Options=defaults
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (Mount device change)", got)
	}
}

func TestCompareUnits_NewSectionAdded_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
[Timer]
OnCalendar=daily
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (new non-Unit section added)", got)
	}
}

func TestCompareUnits_SectionRemoved_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
[Timer]
OnCalendar=daily
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (section removed)", got)
	}
}

func TestCompareUnits_NewKeyInService_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
Restart=always
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (new key in [Service])", got)
	}
}

func TestCompareUnits_RemovedKeyInService_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
Restart=always
`)
	new := []byte(`[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (key removed from [Service])", got)
	}
}

func TestCompareUnits_WhitespaceInsignificant(t *testing.T) {
	// Leading/trailing whitespace around values should be trimmed.
	old := []byte("[Service]\nExecStart=/bin/foo\n")
	new := []byte("[Service]\nExecStart = /bin/foo\n")
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (whitespace around = is trimmed)", got)
	}
}

func TestCompareUnits_EmptyBothSides(t *testing.T) {
	got, err := compareUnitContents(nil, nil)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents(nil, nil) = %v, want UnitEqual", got)
	}
}

func TestCompareUnits_NewUnitSectionWithNonIgnoredKey_NeedsRestart(t *testing.T) {
	old := []byte(`[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Requires=network.target
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (new [Unit] with non-ignored key)", got)
	}
}

func TestCompareUnits_UnitKeyRemovedNonIgnored_NeedsRestart(t *testing.T) {
	old := []byte(`[Unit]
After=network.target
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Description=Foo
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitNeedsRestart {
		t.Errorf("compareUnitContents = %v, want UnitNeedsRestart (After key removed, which is not ignorable)", got)
	}
}

func TestCompareUnits_OnlyIgnoredKeysChanged_Equal(t *testing.T) {
	old := []byte(`[Unit]
Description=Old
Documentation=old
OnFailure=old.target
OnSuccess=old.target
SourcePath=/old
[Service]
ExecStart=/bin/foo
`)
	new := []byte(`[Unit]
Description=New
Documentation=new
OnFailure=new.target
OnSuccess=new.target
SourcePath=/new
[Service]
ExecStart=/bin/foo
`)
	got, err := compareUnitContents(old, new)
	if err != nil {
		t.Fatalf("compareUnitContents() error = %v", err)
	}
	if got != UnitEqual {
		t.Errorf("compareUnitContents = %v, want UnitEqual (all changed keys are ignored)", got)
	}
}

// ---------------------------------------------------------------------------
// Integration: computeDeltas with semantic comparison
// ---------------------------------------------------------------------------

func TestComputeDeltas_InstallOnlyChange_NoAction(t *testing.T) {
	old := map[string][]byte{
		"foo.service": []byte("[Service]\nExecStart=/bin/foo\n[Install]\nWantedBy=multi-user.target\n"),
	}
	new := map[string][]byte{
		"foo.service": []byte("[Service]\nExecStart=/bin/foo\n[Install]\nWantedBy=graphical.target\n"),
	}

	deltas, err := computeDeltas(old, new)
	if err != nil {
		t.Fatalf("computeDeltas() error = %v", err)
	}

	if len(deltas.UnitsToRestart) != 0 || len(deltas.UnitsToReload) != 0 ||
		len(deltas.UnitsToStart) != 0 || len(deltas.UnitsToStop) != 0 {
		t.Errorf("expected no deltas for Install-only change, got start=%v stop=%v restart=%v reload=%v",
			deltas.UnitsToStart, deltas.UnitsToStop, deltas.UnitsToRestart, deltas.UnitsToReload)
	}
}

func TestComputeDeltas_DescriptionOnlyChange_NoAction(t *testing.T) {
	old := map[string][]byte{
		"foo.service": []byte("[Unit]\nDescription=Old\n[Service]\nExecStart=/bin/foo\n"),
	}
	new := map[string][]byte{
		"foo.service": []byte("[Unit]\nDescription=New\n[Service]\nExecStart=/bin/foo\n"),
	}

	deltas, err := computeDeltas(old, new)
	if err != nil {
		t.Fatalf("computeDeltas() error = %v", err)
	}

	if len(deltas.UnitsToRestart) != 0 || len(deltas.UnitsToReload) != 0 {
		t.Errorf("expected no action for Description-only change, got restart=%v reload=%v",
			deltas.UnitsToRestart, deltas.UnitsToReload)
	}
}

func TestComputeDeltas_MountOptionsChange_Reload(t *testing.T) {
	old := map[string][]byte{
		"data.mount": []byte("[Mount]\nWhat=/dev/sda1\nWhere=/mnt/data\nOptions=defaults\n"),
	}
	new := map[string][]byte{
		"data.mount": []byte("[Mount]\nWhat=/dev/sda1\nWhere=/mnt/data\nOptions=defaults,noatime\n"),
	}

	deltas, err := computeDeltas(old, new)
	if err != nil {
		t.Fatalf("computeDeltas() error = %v", err)
	}

	if len(deltas.UnitsToRestart) != 0 {
		t.Errorf("UnitsToRestart = %v, want empty (mount options -> reload)", deltas.UnitsToRestart)
	}
	if !reflect.DeepEqual(deltas.UnitsToReload, []string{"data.mount"}) {
		t.Errorf("UnitsToReload = %v, want [data.mount]", deltas.UnitsToReload)
	}
}

// ---------------------------------------------------------------------------
// Integration: generateDeltas with files on disk
// ---------------------------------------------------------------------------

func TestGenerateDeltas_DescriptionChangeOnDisk_NoAction(t *testing.T) {
	oldDir := t.TempDir()
	newDir := t.TempDir()

	os.WriteFile(filepath.Join(oldDir, "app.service"),
		[]byte("[Unit]\nDescription=Old App\n[Service]\nExecStart=/bin/app\n"), 0644)
	os.WriteFile(filepath.Join(newDir, "app.service"),
		[]byte("[Unit]\nDescription=New App\n[Service]\nExecStart=/bin/app\n"), 0644)

	deltas, err := generateDeltas(oldDir, newDir)
	if err != nil {
		t.Fatalf("generateDeltas() error = %v", err)
	}

	if len(deltas.UnitsToRestart) != 0 || len(deltas.UnitsToReload) != 0 {
		t.Errorf("expected no action for description-only change, got restart=%v reload=%v",
			deltas.UnitsToRestart, deltas.UnitsToReload)
	}
}
