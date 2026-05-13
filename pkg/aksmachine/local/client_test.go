package local

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

func TestClientCreateGetAndPatchStatus(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "machine.json")
	client, err := NewClient(path)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	created, err := client.Create(context.Background(), aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Goal.SettingsVersion != "42" {
		t.Fatalf("created goal = %#v", created.Goal)
	}
	if created.ID != localResourceID {
		t.Fatalf("created ID = %q", created.ID)
	}

	if err := client.PatchStatus(context.Background(), aksmachine.Status{ProvisioningState: aksmachine.ProvisioningStateSucceeded, ObservedSettingsVersion: "42"}); err != nil {
		t.Fatalf("PatchStatus: %v", err)
	}

	got, err := client.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Goal.KubernetesVersion != "1.34.0" || got.Status.ProvisioningState != aksmachine.ProvisioningStateSucceeded {
		t.Fatalf("got machine = %#v", got)
	}
}

func TestClientGetNotFound(t *testing.T) {
	t.Parallel()

	client, err := NewClient(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Get(context.Background())
	var notFound *aksmachine.NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Get error = %v, want NotFoundError", err)
	}
}

func TestClientReadsExternalMutation(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "machine.json")
	client, err := NewClient(path)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Create(context.Background(), aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "42"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mutated := aksmachine.Machine{Goal: aksmachine.GoalState{KubernetesVersion: "1.35.0", SettingsVersion: "43"}}
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
