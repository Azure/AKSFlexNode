package drift

import "testing"

func TestDefaultDetectors(t *testing.T) {
	t.Parallel()

	d := DefaultDetectors()
	if len(d) == 0 {
		t.Fatalf("DefaultDetectors returned empty")
	}
	if d[0] == nil {
		t.Fatalf("DefaultDetectors[0] is nil")
	}
}
