package spec

import (
	"strings"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

// DesiredKubernetesVersion returns the preferred Kubernetes version from a managed cluster spec.
//
// Priority:
//  1. CurrentKubernetesVersion (typically a full patch version)
//  2. KubernetesVersion (typically major.minor or a less specific version)
func DesiredKubernetesVersion(specSnap *ManagedClusterSpec) string {
	if specSnap == nil {
		return ""
	}
	desired := strings.TrimSpace(specSnap.CurrentKubernetesVersion)
	if desired == "" {
		desired = strings.TrimSpace(specSnap.KubernetesVersion)
	}
	return desired
}

// OverrideKubernetesVersionFromManagedClusterSpec loads the managed cluster spec snapshot from disk
// and, if it contains a Kubernetes version, overwrites cfg.Kubernetes.Version.
//
// This is best-effort: callers may choose to ignore errors if the spec file doesn't exist yet.
func OverrideKubernetesVersionFromManagedClusterSpec(cfg *config.Config) (changed bool, oldVersion, newVersion string, err error) {
	return OverrideKubernetesVersionFromManagedClusterSpecFile(cfg, GetManagedClusterSpecFilePath())
}

// OverrideKubernetesVersionFromManagedClusterSpecFile loads the spec snapshot from the given file path
// and overwrites cfg.Kubernetes.Version if a desired Kubernetes version is present.
func OverrideKubernetesVersionFromManagedClusterSpecFile(cfg *config.Config, path string) (changed bool, oldVersion, newVersion string, err error) {
	if cfg == nil {
		return false, "", "", nil
	}

	specSnap, err := LoadManagedClusterSpecFromFile(path)
	if err != nil || specSnap == nil {
		return false, "", "", err
	}

	desired := DesiredKubernetesVersion(specSnap)
	if desired == "" {
		return false, "", "", nil
	}

	old := cfg.Kubernetes.Version
	if old == desired {
		return false, old, desired, nil
	}

	cfg.Kubernetes.Version = desired
	return true, old, desired, nil
}
