package nspawnlifecycle

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestLoadConfigLoadsAndConvertsPersistedConfig(t *testing.T) {
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
		"bootstrap": {"additionalRequiredServices": ["storage-ready.service"]},
		"node": {"kubelet": {
			"clusterFQDN": "test-cluster.hcp.eastus.azmk8s.io",
			"caCertData": "LS0tLS1CRUdJTi1DRVJUSUZJQ0FURS0tLS0t"
		}}
	}`
	if err := os.WriteFile(path, []byte(configJSON), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	for _, machine := range []string{"kube1", "kube2"} {
		t.Run(machine, func(t *testing.T) {
			t.Parallel()

			for range 2 {
				cfg, services, err := loadConfig(path, machine)
				if err != nil {
					t.Fatalf("load config: %v", err)
				}
				if cfg == nil {
					t.Fatal("persisted config was not found")
				}
				if cfg.MachineName != machine {
					t.Errorf("MachineName = %q, want %s", cfg.MachineName, machine)
				}
				if !slices.Equal(services, []string{"storage-ready.service"}) {
					t.Errorf("required services = %v, want storage-ready.service", services)
				}
				if cfg.NodeName != "flex-node-1" {
					t.Errorf("NodeName = %q, want flex-node-1", cfg.NodeName)
				}
				if cfg.Kubelet.Auth.BootstrapToken == "" {
					t.Error("bootstrap token was not converted")
				}
				kernelLogMounts := 0
				for _, mount := range cfg.AdditionalHostMounts {
					if mount.Source == "/dev/kmsg" {
						kernelLogMounts++
						if !mount.ReadOnly || (mount.Target != "" && mount.Target != "/dev/kmsg") {
							t.Error("NPD kernel-log mount must expose the real device read-only")
						}
					}
				}
				if kernelLogMounts != 1 {
					t.Errorf("NPD kernel-log mounts = %d, want 1", kernelLogMounts)
				}
			}
		})
	}
}

func TestLoadConfigTreatsMissingConfigAsBootstrapWindow(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing.json")
	for _, machine := range []string{"kube1", "kube2"} {
		t.Run(machine, func(t *testing.T) {
			t.Parallel()

			cfg, services, err := loadConfig(path, machine)
			if err != nil {
				t.Fatalf("load missing config: %v", err)
			}
			if cfg != nil || services != nil {
				t.Fatal("missing config returned configuration or required services")
			}

			lifecycle, err := newLifecycle(slog.New(slog.DiscardHandler), path)
			if err != nil {
				t.Fatalf("create lifecycle: %v", err)
			}
			for range 2 {
				if err := lifecycle.PreStart(t.Context(), machine); err != nil {
					t.Fatalf("pre-start during bootstrap window: %v", err)
				}
			}
		})
	}
}

func TestLoadConfigRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		configJSON string
		directory  bool
		wantError  string
	}{
		{name: "malformed JSON", configJSON: "{", wantError: "error unmarshaling config"},
		{name: "validation failure", configJSON: "{}", wantError: "config validation failed"},
		{name: "read failure", directory: true, wantError: "failed to read config file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "config.json")
			if tt.directory {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(tt.configJSON), 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, services, err := loadConfig(path, "kube1")
			if err == nil || !strings.Contains(err.Error(), "load persisted AKS Flex config "+path) || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("load invalid config error = %v, want path context and %q", err, tt.wantError)
			}
			if cfg != nil || services != nil {
				t.Fatal("invalid config returned configuration or required services")
			}

			lifecycle, err := newLifecycle(slog.New(slog.DiscardHandler), path)
			if err != nil {
				t.Fatalf("create lifecycle: %v", err)
			}
			if err := lifecycle.PreStart(t.Context(), "kube1"); err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("pre-start accepted invalid config: %v", err)
			}
		})
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
