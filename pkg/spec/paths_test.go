package spec

import "testing"

func TestManagedClusterSpecFilePath(t *testing.T) {
	t.Parallel()

	dir := "/some/dir"
	got := ManagedClusterSpecFilePath(dir)
	want := dir + "/managedcluster-spec.json"
	if got != want {
		t.Fatalf("ManagedClusterSpecFilePath()=%q, want %q", got, want)
	}
}
