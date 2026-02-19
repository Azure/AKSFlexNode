package drift

import (
	"strconv"
	"strings"
)

func majorMinor(version string) string {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func parseMajorMinor(version string) (major int, minor int, ok bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	maj, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	min, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
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
