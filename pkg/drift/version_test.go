package drift

import "testing"

func TestMajorMinor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    string
	}{
		{name: "trim-v", version: "v1.30.2", want: "1.30"},
		{name: "already-major-minor", version: "1.30", want: "1.30"},
		{name: "only-major", version: "1", want: "1"},
		{name: "spaces", version: "  v1.31.7  ", want: "1.31"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := majorMinor(tt.version); got != tt.want {
				t.Fatalf("majorMinor(%q)=%q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func TestParseMajorMinor(t *testing.T) {
	t.Parallel()

	maj, min, ok := parseMajorMinor("v1.31.7")
	if !ok || maj != 1 || min != 31 {
		t.Fatalf("parseMajorMinor(v1.31.7)=(%d,%d,%v), want (1,31,true)", maj, min, ok)
	}

	_, _, ok = parseMajorMinor("1")
	if ok {
		t.Fatalf("parseMajorMinor(1) ok=true, want false")
	}
	_, _, ok = parseMajorMinor("foo.1")
	if ok {
		t.Fatalf("parseMajorMinor(foo.1) ok=true, want false")
	}
	_, _, ok = parseMajorMinor("1.bar")
	if ok {
		t.Fatalf("parseMajorMinor(1.bar) ok=true, want false")
	}
}

func TestCompareMajorMinor(t *testing.T) {
	t.Parallel()

	cmp, ok := compareMajorMinor("1.29.5", "1.30.0")
	if !ok || cmp != -1 {
		t.Fatalf("compareMajorMinor(1.29.5,1.30.0)=(%d,%v), want (-1,true)", cmp, ok)
	}

	cmp, ok = compareMajorMinor("1.30.0", "1.30.7")
	if !ok || cmp != 0 {
		t.Fatalf("compareMajorMinor(1.30.0,1.30.7)=(%d,%v), want (0,true)", cmp, ok)
	}

	cmp, ok = compareMajorMinor("1.31.0", "1.30.9")
	if !ok || cmp != 1 {
		t.Fatalf("compareMajorMinor(1.31.0,1.30.9)=(%d,%v), want (1,true)", cmp, ok)
	}

	_, ok = compareMajorMinor("1.x", "1.30.0")
	if ok {
		t.Fatalf("compareMajorMinor(1.x,1.30.0) ok=true, want false")
	}
}
