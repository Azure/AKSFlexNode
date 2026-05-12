package drift

import "testing"

func TestParseMajorMinor(t *testing.T) {
	t.Parallel()

	v, ok := parseMajorMinor("v1.31.7")
	if !ok || v.Major != 1 || v.Minor != 31 || v.Patch != 0 {
		t.Fatalf("parseMajorMinor(v1.31.7)=(%s,%v), want (1.31.0,true)", v, ok)
	}

	_, ok = parseMajorMinor("1")
	if ok {
		t.Fatalf("parseMajorMinor(1) ok=true, want false")
	}
	_, ok = parseMajorMinor("foo.1")
	if ok {
		t.Fatalf("parseMajorMinor(foo.1) ok=true, want false")
	}
	_, ok = parseMajorMinor("1.bar")
	if ok {
		t.Fatalf("parseMajorMinor(1.bar) ok=true, want false")
	}
}

func TestParseMajorMinor_Overflow(t *testing.T) {
	t.Parallel()

	// semver parsing stores major/minor as uint64; no lossy int conversion is needed.
	v, ok := parseMajorMinor("9223372036854775808.1.0")
	if !ok || v.Major != 9223372036854775808 || v.Minor != 1 || v.Patch != 0 {
		t.Fatalf("parseMajorMinor(large)=(%s,%v), want (9223372036854775808.1.0,true)", v, ok)
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
