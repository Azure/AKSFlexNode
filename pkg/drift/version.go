package drift

import (
	"strings"

	"github.com/blang/semver/v4"
)

func parseMajorMinor(version string) (semver.Version, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v == "" {
		return semver.Version{}, false
	}

	// Preserve legacy expectations: require at least major.minor.
	// semver.ParseTolerant accepts "1" as "1.0.0", but callers rely on "1" being treated as non-semver.
	if strings.Count(v, ".") == 0 {
		return semver.Version{}, false
	}
	// semver requires major.minor.patch; tolerate "major.minor" by appending ".0".
	if strings.Count(v, ".") == 1 {
		v = v + ".0"
	}
	parsed, err := semver.ParseTolerant(v)
	if err != nil {
		return semver.Version{}, false
	}

	return semver.Version{Major: parsed.Major, Minor: parsed.Minor}, true
}

// compareMajorMinor compares versions by major.minor only.
// Returns -1 if current < desired, 0 if equal, +1 if current > desired, and ok=false if parsing fails.
func compareMajorMinor(current, desired string) (cmp int, ok bool) {
	currentVersion, ok1 := parseMajorMinor(current)
	desiredVersion, ok2 := parseMajorMinor(desired)
	if !ok1 || !ok2 {
		return 0, false
	}
	return currentVersion.Compare(desiredVersion), true
}
