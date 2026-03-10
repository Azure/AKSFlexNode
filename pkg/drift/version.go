package drift

import (
	"fmt"
	"strings"

	"github.com/blang/semver/v4"
)

func safeUint64ToInt(v uint64) (int, bool) {
	maxInt := uint64(^uint(0) >> 1)
	if v > maxInt {
		return 0, false
	}
	return int(v), true
}

func majorMinor(version string) string {
	maj, min, ok := parseMajorMinor(version)
	if ok {
		return fmt.Sprintf("%d.%d", maj, min)
	}

	// Best-effort fallback to preserve legacy behavior for weird strings.
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func parseMajorMinor(version string) (major int, minor int, ok bool) {
	v, err := parseTolerantSemver(version)
	if err != nil {
		return 0, 0, false
	}

	maj, ok := safeUint64ToInt(v.Major)
	if !ok {
		return 0, 0, false
	}
	min, ok := safeUint64ToInt(v.Minor)
	if !ok {
		return 0, 0, false
	}
	return maj, min, true
}

func parseTolerantSemver(version string) (semver.Version, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" {
		return semver.Version{}, fmt.Errorf("empty version")
	}
	// Preserve legacy expectations: require at least major.minor.
	// semver.ParseTolerant accepts "1" as "1.0.0", but callers rely on "1" being treated as non-semver.
	if strings.Count(v, ".") == 0 {
		return semver.Version{}, fmt.Errorf("missing minor version")
	}
	// semver requires major.minor.patch; tolerate "major.minor" by appending ".0".
	if strings.Count(v, ".") == 1 {
		v = v + ".0"
	}
	return semver.ParseTolerant(v)
}

// compareMajorMinor compares versions by major.minor only.
// Returns -1 if current < desired, 0 if equal, +1 if current > desired, and ok=false if parsing fails.
func compareMajorMinor(current, desired string) (cmp int, ok bool) {
	cMaj, cMin, ok1 := parseMajorMinor(current)
	dMaj, dMin, ok2 := parseMajorMinor(desired)
	if !ok1 || !ok2 {
		return 0, false
	}
	if cMaj != dMaj {
		if cMaj < dMaj {
			return -1, true
		}
		return 1, true
	}
	if cMin != dMin {
		if cMin < dMin {
			return -1, true
		}
		return 1, true
	}
	return 0, true
}
