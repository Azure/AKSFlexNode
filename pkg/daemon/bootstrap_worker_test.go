package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestContainerizedAgentModeValidation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		mode          isolation.Mode
		containerized bool
		wantErr       bool
	}{
		{
			name:          "containerized nspawn",
			mode:          isolation.ModeNSpawn,
			containerized: true,
		},
		{
			name:          "containerized none",
			mode:          isolation.ModeNone,
			containerized: true,
			wantErr:       true,
		},
		{
			name:          "containerized OCI",
			mode:          isolation.ModeContainer,
			containerized: true,
			wantErr:       true,
		},
		{
			name: "host OCI",
			mode: isolation.ModeContainer,
		},
		{
			name: "host none",
			mode: isolation.ModeNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := ValidateAgentDeployment(tc.mode, tc.containerized); (err != nil) != tc.wantErr {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestValidateWorkerBootstrapCleanHostGuard(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		mode          isolation.Mode
		applied       *State
		containerized bool
		cleanErr      error
		wantClean     bool
		wantErr       string
	}{
		{
			name:      "fresh OCI clean host",
			mode:      isolation.ModeContainer,
			wantClean: true,
		},
		{
			name:      "fresh OCI bpffs conflict",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("bpffs mount path /run/bpffs/kube1"),
			wantClean: true,
			wantErr:   "bpffs mount path",
		},
		{
			name:      "fresh OCI nspawn conflict",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("registered nspawn machine kube2"),
			wantClean: true,
			wantErr:   "registered nspawn machine",
		},
		{
			name:      "fresh OCI inspection error",
			mode:      isolation.ModeContainer,
			cleanErr:  errors.New("artifact cannot be inspected"),
			wantClean: true,
			wantErr:   "artifact cannot be inspected",
		},
		{
			name: "installed OCI kube1",
			mode: isolation.ModeContainer,
			applied: &State{
				Isolation:     isolation.ModeContainer,
				ActiveMachine: "kube1",
			},
			cleanErr: errors.New("owned bpffs mount path /run/bpffs/kube1"),
		},
		{
			name: "installed OCI kube2",
			mode: isolation.ModeContainer,
			applied: &State{
				Isolation:     isolation.ModeContainer,
				ActiveMachine: "kube2",
			},
			cleanErr: errors.New("owned bpffs mount path /run/bpffs/kube2"),
		},
		{
			name:    "wrong applied mode",
			mode:    isolation.ModeContainer,
			applied: &State{Isolation: isolation.ModeNSpawn},
			wantErr: "isolation change requires reset",
		},
		{
			name:          "containerized agent",
			mode:          isolation.ModeContainer,
			containerized: true,
			wantErr:       "containerized agent does not yet support",
		},
		{
			name:      "none still checks clean host",
			mode:      isolation.ModeNone,
			applied:   &State{Isolation: isolation.ModeNone},
			cleanErr:  errors.New("nspawn artifact"),
			wantClean: true,
			wantErr:   "nspawn artifact",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Bootstrap: config.BootstrapConfig{Isolation: tc.mode},
				Agent:     config.AgentConfig{NodeName: "node1"},
				Azure: config.AzureConfig{
					BootstrapToken: &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
				},
			}
			if tc.applied != nil && tc.applied.ActiveMachine != "" {
				tc.applied.NodeConfig = config.ToAgentConfig(cfg, tc.applied.ActiveMachine)
			}
			log := slog.New(slog.DiscardHandler)
			calls := 0
			ensureClean := func(ctx context.Context, gotLog *slog.Logger, mode isolation.Mode) error {
				calls++
				if ctx != t.Context() || gotLog != log || mode != isolation.ModeNSpawn {
					t.Fatalf("clean-host check received unexpected inputs: mode=%q", mode)
				}
				return tc.cleanErr
			}

			err := validateWorkerBootstrap(t.Context(), cfg, log, tc.applied, tc.containerized, ensureClean)
			if (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("validation error = %v, want %q", err, tc.wantErr)
			}
			wantCalls := 0
			if tc.wantClean {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("clean-host calls = %d, want %d", calls, wantCalls)
			}
		})
	}
}

func TestValidateBootstrapArtifactConflicts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		applied *State
		none    bool
		oci     bool
		wantErr string
	}{
		{
			name:    "orphan OCI",
			mode:    isolation.ModeContainer,
			oci:     true,
			wantErr: "OCI worker has no applied state",
		},
		{
			name:    "none conflicts with OCI",
			mode:    isolation.ModeContainer,
			applied: &State{Isolation: isolation.ModeContainer},
			none:    true,
			oci:     true,
			wantErr: "host-native worker is present",
		},
		{
			name:    "orphan none",
			mode:    isolation.ModeNone,
			none:    true,
			wantErr: "host-native worker has no applied state",
		},
		{
			name:    "OCI conflicts with none",
			mode:    isolation.ModeNone,
			oci:     true,
			wantErr: "OCI worker is present",
		},
		{
			name:    "OCI conflicts with nspawn",
			mode:    isolation.ModeNSpawn,
			oci:     true,
			wantErr: "OCI worker is present",
		},
		{
			name:    "none conflicts with nspawn",
			mode:    isolation.ModeNSpawn,
			none:    true,
			wantErr: "host-native worker is present",
		},
		{
			name:    "owned OCI artifacts",
			mode:    isolation.ModeContainer,
			applied: &State{Isolation: isolation.ModeContainer},
			oci:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			ensureClean := func(context.Context, *slog.Logger, isolation.Mode) error {
				calls++
				return errors.New("unexpected clean-host check")
			}
			err := validateBootstrapArtifacts(t.Context(), tc.mode, slog.New(slog.DiscardHandler), tc.applied, tc.none, tc.oci, ensureClean)
			if (tc.wantErr == "" && err != nil) || (tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr))) {
				t.Fatalf("artifact validation error = %v, want %q", err, tc.wantErr)
			}
			if calls != 0 {
				t.Fatalf("unexpected clean-host calls = %d", calls)
			}
		})
	}
}

func TestOCIStateLoadRejectsInvalidAppliedConfiguration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		slot   string
		config *agentconfig.AgentConfig
	}{
		{
			name: "invalid slot",
			slot: "kube3",
			config: &agentconfig.AgentConfig{
				Isolation: string(isolation.ModeContainer),
			},
		},
		{
			name: "missing config",
			slot: "kube1",
		},
		{
			name: "wrong config mode",
			slot: "kube2",
			config: &agentconfig.AgentConfig{
				Isolation: string(isolation.ModeNone),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, err := newFileStateStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			applied := &State{
				Isolation:     isolation.ModeContainer,
				ActiveMachine: tc.slot,
				NodeConfig:    tc.config,
			}
			if err := store.Save(t.Context(), applied); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Load(t.Context()); err == nil || !strings.Contains(err.Error(), "applied OCI worker has invalid slot or runtime configuration") {
				t.Fatalf("load error = %v", err)
			}
		})
	}
}

func TestBootstrapStateSelectsInstalledWorker(t *testing.T) {
	t.Parallel()

	seed := &State{ActiveMachine: "kube1"}
	cfg := &config.Config{
		Agent: config.AgentConfig{NodeName: "node1"},
		Azure: config.AzureConfig{
			BootstrapToken: &config.BootstrapTokenConfig{Token: "abcdef.0123456789abcdef"},
		},
	}
	installed := &State{ActiveMachine: "kube1", NodeConfig: config.ToAgentConfig(cfg, "kube1")}
	alternate := &State{ActiveMachine: "kube2", NodeConfig: config.ToAgentConfig(cfg, "kube2")}
	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		applied *State
		want    *State
		reapply bool
		wantErr bool
	}{
		{
			name: "first install",
			mode: isolation.ModeNone,
			want: seed,
		},
		{
			name:    "none reapply",
			mode:    isolation.ModeNone,
			applied: installed,
			want:    installed,
			reapply: true,
		},
		{
			name:    "OCI alternate slot",
			mode:    isolation.ModeContainer,
			applied: alternate,
			want:    alternate,
			reapply: true,
		},
		{
			name:    "nspawn keeps bootstrap behavior",
			mode:    isolation.ModeNSpawn,
			applied: installed,
			want:    seed,
		},
		{
			name:    "missing applied config",
			mode:    isolation.ModeNone,
			applied: &State{ActiveMachine: "kube1"},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			candidate := cfg.DeepCopy()
			candidate.Bootstrap.Isolation = tc.mode
			got, reapply, err := BootstrapState(tc.applied, seed, candidate)
			if (err != nil) != tc.wantErr || got != tc.want || reapply != tc.reapply {
				t.Fatalf("selection = %p, %t, %v; want %p, %t", got, reapply, err, tc.want, tc.reapply)
			}
		})
	}
}

func TestNoneRestartUsesAppliedConfigurationWithoutResolvingDrift(t *testing.T) {
	t.Parallel()

	applied := &agentconfig.AgentConfig{
		Isolation: string(isolation.ModeNone),
		Cluster:   agentconfig.AgentClusterConfig{Version: "1.34.0"},
	}
	worker := &fakeWorkerRuntime{}
	h := &machineOperationHandlers{
		log:     slog.New(slog.DiscardHandler),
		runtime: worker,
		state: &testStateStore{
			state: &State{
				Isolation:     isolation.ModeNone,
				ActiveMachine: "kube1",
				NodeConfig:    applied,
			},
		},
		cfg: &config.Config{
			Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
		},
		resolve: func(context.Context, *slog.Logger, *config.Config, string) (config.ResolvedMachineGoal, error) {
			t.Fatal("resolved caller configuration for none restart")
			return config.ResolvedMachineGoal{}, nil
		},
	}
	if err := h.restartNode(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !worker.restarted || worker.restartInput.Node.Config != applied {
		t.Fatal("restart did not use the installed SDK configuration")
	}
}
