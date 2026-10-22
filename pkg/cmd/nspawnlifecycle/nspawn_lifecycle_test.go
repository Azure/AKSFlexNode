package nspawnlifecycle

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

type fakeLifecycle struct {
	operation string
	machine   string
	err       error
}

func (f *fakeLifecycle) PreStart(_ context.Context, machine string) error {
	f.operation = "pre-start"
	f.machine = machine
	return f.err
}

func (f *fakeLifecycle) PostStart(_ context.Context, machine string) error {
	f.operation = "post-start"
	f.machine = machine
	return f.err
}

func (f *fakeLifecycle) Reconcile(_ context.Context, machine string) error {
	f.operation = "reconcile"
	f.machine = machine
	return f.err
}

func TestCommandRegistersLifecycleOperations(t *testing.T) {
	t.Parallel()

	cmd := NewCommand()
	if !cmd.Hidden {
		t.Fatal("nspawn lifecycle command must be hidden")
	}

	var got []string
	for _, child := range cmd.Commands() {
		got = append(got, child.Name())
		if !child.Hidden {
			t.Errorf("operation %q must be hidden", child.Name())
		}
	}
	for _, want := range []string{"post-start", "pre-start", "reconcile"} {
		if !slices.Contains(got, want) {
			t.Errorf("registered operations = %v, missing %q", got, want)
		}
	}
}

func TestCommandValidatesMachine(t *testing.T) {
	t.Parallel()

	factoryCalled := false
	cmd := newCommand(slog.New(slog.DiscardHandler), func(*slog.Logger) (lifecycle, error) {
		factoryCalled = true
		return &fakeLifecycle{}, nil
	}, migrated)
	cmd.SetArgs([]string{"pre-start", "not-a-machine"})

	err := cmd.ExecuteContext(context.Background())
	if err == nil || err.Error() != `unknown nspawn machine "not-a-machine"` {
		t.Fatalf("ExecuteContext() error = %v, want unknown-machine error", err)
	}
	if factoryCalled {
		t.Fatal("lifecycle factory called for invalid machine")
	}
}

func TestConfigLoaderLoadsAndConvertsPersistedConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	configJSON := `{
		"azure": {
			"subscriptionId": "12345678-1234-1234-1234-123456789012",
			"tenantId": "12345678-1234-1234-1234-123456789012",
			"cloud": "AzurePublicCloud",
			"targetAgentPoolName": "flexnode-edge",
			"bootstrapToken": {"token": "abcdef.0123456789abcdef"},
			"targetCluster": {
				"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster",
				"location": "eastus"
			}
		},
		"agent": {"nodeName": "flex-node-1", "logLevel": "info"},
		"node": {"kubelet": {
			"clusterFQDN": "test-cluster.hcp.eastus.azmk8s.io",
			"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t"
		}}
	}`
	if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, found, err := configLoader(path)(context.Background(), "kube2")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !found {
		t.Fatal("load config found = false, want true")
	}
	if cfg.MachineName != "kube2" {
		t.Errorf("MachineName = %q, want kube2", cfg.MachineName)
	}
	if cfg.NodeName != "flex-node-1" {
		t.Errorf("NodeName = %q, want flex-node-1", cfg.NodeName)
	}
	if cfg.Kubelet.Auth.BootstrapToken == "" {
		t.Error("bootstrap token was not converted")
	}
}

func TestConfigLoaderTreatsMissingConfigAsBootstrapWindow(t *testing.T) {
	t.Parallel()

	cfg, found, err := configLoader(filepath.Join(t.TempDir(), "missing.json"))(context.Background(), "kube1")
	if err != nil {
		t.Fatalf("load missing config: %v", err)
	}
	if found {
		t.Fatal("load missing config found = true, want false")
	}
	if cfg != nil {
		t.Fatalf("load missing config = %#v, want nil", cfg)
	}
}

func TestCommandDispatchesOperations(t *testing.T) {
	t.Parallel()

	dispatchErr := errors.New("dispatched")
	tests := []struct {
		name      string
		operation string
		// Whether the phase migrates the host root first.
		migrates bool
	}{
		{name: "pre start", operation: "pre-start", migrates: true},
		{name: "post start", operation: "post-start"},
		{name: "reconcile", operation: "reconcile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := &fakeLifecycle{err: dispatchErr}
			migrations := 0
			cmd := newCommand(slog.New(slog.DiscardHandler), func(*slog.Logger) (lifecycle, error) {
				return fake, nil
			}, func(*slog.Logger) error {
				migrations++
				return nil
			})
			cmd.SetArgs([]string{tt.operation, "kube2"})

			err := cmd.ExecuteContext(context.Background())
			if !errors.Is(err, dispatchErr) {
				t.Fatalf("ExecuteContext() error = %v, want %v", err, dispatchErr)
			}
			if fake.operation != tt.operation {
				t.Errorf("dispatched operation = %q, want %q", fake.operation, tt.operation)
			}
			if fake.machine != "kube2" {
				t.Errorf("dispatched machine = %q, want kube2", fake.machine)
			}
			if got := migrations == 1; got != tt.migrates {
				t.Errorf("migrated the host root %d times, want migrated = %v", migrations, tt.migrates)
			}
		})
	}
}

// TestCommandStartsTheMachineWhenTheHostRootCannotBeMigrated: a host root that
// cannot be migrated must not keep the kubelet machine from starting. pre-start
// leaves its config as it is, and the other phases run as usual.
func TestCommandStartsTheMachineWhenTheHostRootCannotBeMigrated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		operation string
		wantRun   bool
	}{
		{operation: "pre-start"},
		{operation: "post-start", wantRun: true},
		{operation: "reconcile", wantRun: true},
	}

	for _, tt := range tests {
		t.Run(tt.operation, func(t *testing.T) {
			t.Parallel()

			fake := &fakeLifecycle{}
			cmd := newCommand(slog.New(slog.DiscardHandler), func(*slog.Logger) (lifecycle, error) {
				return fake, nil
			}, func(*slog.Logger) error {
				return errors.New("the agent is installed under both roots")
			})
			cmd.SetArgs([]string{tt.operation, "kube1"})

			if err := cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatalf("ExecuteContext() error = %v, want the machine to start", err)
			}
			if ran := fake.operation == tt.operation; ran != tt.wantRun {
				t.Errorf("ran %s = %v, want %v", tt.operation, ran, tt.wantRun)
			}
		})
	}
}

func migrated(*slog.Logger) error { return nil }
