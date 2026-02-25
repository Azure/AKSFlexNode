package drift

// DefaultDetectors returns the default set of drift detectors enabled by the agent.
//
// The daemon can choose to use this helper to avoid wiring each detector manually,
// while still keeping detector selection injectable for tests and future customization.
func DefaultDetectors() []Detector {
	return []Detector{
		NewKubernetesVersionDetector(),
	}
}
