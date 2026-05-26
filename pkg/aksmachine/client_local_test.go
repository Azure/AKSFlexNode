//go:build local_e2e

package aksmachine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalClientCreateGetAndPatchStatus(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "machine.json")
	client, err := newLocalClient(path)
	if err != nil {
		t.Fatalf("newLocalClient: %v", err)
	}

	created, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Goal.SettingsVersion != "42" {
		t.Fatalf("created goal = %#v", created.Goal)
	}
	if created.ID != localResourceID {
		t.Fatalf("created ID = %q", created.ID)
	}

	if err := client.PatchStatus(context.Background(), Status{ProvisioningState: ProvisioningStateSucceeded, ObservedSettingsVersion: "42"}); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}

	got, err := client.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Goal.KubernetesVersion != "1.34.0" || got.Status.ProvisioningState != ProvisioningStateSucceeded {
		t.Fatalf("got machine = %#v", got)
	}
}

func TestLocalClientGetNotFound(t *testing.T) {
	t.Parallel()

	client, err := newLocalClient(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("newLocalClient: %v", err)
	}
	_, err = client.Get(context.Background())
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Get error = %v, want NotFoundError", err)
	}
}

func TestLocalClientReadsExternalMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "machine.json")
	client, err := newLocalClient(path)
	if err != nil {
		t.Fatalf("newLocalClient: %v", err)
	}
	if _, err := client.Create(context.Background(), GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mutated := Machine{Goal: GoalState{KubernetesVersion: "1.35.0", SettingsVersion: "43"}}
	data, err := json.Marshal(mutated)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := client.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Goal.SettingsVersion != "43" {
		t.Fatalf("SettingsVersion = %q", got.Goal.SettingsVersion)
	}
}
