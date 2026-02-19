package spec

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"go.goms.io/aks/AKSFlexNode/pkg/config"
)

func TestDesiredKubernetesVersion(t *testing.T) {
	t.Parallel()

	if got := DesiredKubernetesVersion(nil); got != "" {
		t.Fatalf("DesiredKubernetesVersion(nil)=%q, want empty", got)
	}

	if got := DesiredKubernetesVersion(&ManagedClusterSpec{CurrentKubernetesVersion: " 1.30.7 ", KubernetesVersion: "1.30"}); got != "1.30.7" {
		t.Fatalf("DesiredKubernetesVersion(current+fallback)=%q, want %q", got, "1.30.7")
	}
	if got := DesiredKubernetesVersion(&ManagedClusterSpec{CurrentKubernetesVersion: " ", KubernetesVersion: " 1.31 "}); got != "1.31" {
		t.Fatalf("DesiredKubernetesVersion(fallback)=%q, want %q", got, "1.31")
	}
	if got := DesiredKubernetesVersion(&ManagedClusterSpec{CurrentKubernetesVersion: " ", KubernetesVersion: " "}); got != "" {
		t.Fatalf("DesiredKubernetesVersion(empty)=%q, want empty", got)
	}
}

func TestOverrideKubernetesVersionFromManagedClusterSpec(t *testing.T) {
	dir := t.TempDir()
	path := ManagedClusterSpecFilePath(dir)

	// Missing spec file: should return an error from LoadManagedClusterSpec.
	cfg := &config.Config{Kubernetes: config.KubernetesConfig{Version: "1.29.0"}}
	changed, oldV, newV, err := OverrideKubernetesVersionFromManagedClusterSpecFile(cfg, path)
	if err == nil {
		t.Fatalf("expected error when spec file missing")
	}
	if changed {
		t.Fatalf("changed=true, want false")
	}
	if oldV != "" || newV != "" {
		t.Fatalf("old/new versions should be empty on error; old=%q new=%q", oldV, newV)
	}

	// Write a spec snapshot file with desired version.
	snap := &ManagedClusterSpec{
		SchemaVersion:            ManagedClusterSpecSchemaVersion,
		KubernetesVersion:        "1.30",
		CurrentKubernetesVersion: "1.30.7",
		CollectedAt:              time.Now(),
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	changed, oldV, newV, err = OverrideKubernetesVersionFromManagedClusterSpecFile(cfg, path)
	if err != nil {
		t.Fatalf("OverrideKubernetesVersionFromManagedClusterSpec() err=%v, want nil", err)
	}
	if !changed {
		t.Fatalf("changed=false, want true")
	}
	if oldV != "1.29.0" || newV != "1.30.7" {
		t.Fatalf("old/new mismatch: old=%q new=%q", oldV, newV)
	}
	if cfg.Kubernetes.Version != "1.30.7" {
		t.Fatalf("cfg.Kubernetes.Version=%q, want %q", cfg.Kubernetes.Version, "1.30.7")
	}

	// Second call should be a no-op.
	changed, oldV, newV, err = OverrideKubernetesVersionFromManagedClusterSpecFile(cfg, path)
	if err != nil {
		t.Fatalf("second override err=%v, want nil", err)
	}
	if changed {
		t.Fatalf("second override changed=true, want false")
	}
	if oldV != "1.30.7" || newV != "1.30.7" {
		t.Fatalf("old/new mismatch on no-op: old=%q new=%q", oldV, newV)
	}
}
