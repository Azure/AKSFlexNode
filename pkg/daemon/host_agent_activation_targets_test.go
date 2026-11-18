package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type activationTargetService struct {
	targets      agentdeployment.ServiceTargets
	active       bool
	preflightErr error
	preflights   int
	prepares     int
	restarts     int
}

func (s *activationTargetService) Preflight(_ context.Context, _ string, targets agentdeployment.ServiceTargets) (agentbinary.ServicePlan, error) {
	s.preflights++
	s.targets = targets

	return agentbinary.ServicePlan{UpdateRequired: true}, s.preflightErr
}

func (s *activationTargetService) Prepare(context.Context, string) error {
	s.prepares++

	return nil
}

func (*activationTargetService) Reload(context.Context) error { return nil }

func (s *activationTargetService) Restart(context.Context) error {
	if s.active {
		s.restarts++
	}

	return nil
}

func (*activationTargetService) WaitHealthyForBinary(_ context.Context, path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func writeActivationConfig(t *testing.T, path, overrides string) string {
	t.Helper()

	const baseline = `{
		"azure": {
			"targetCluster": {"resourceId": "/subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/cluster"},
			"bootstrapToken": {"token": "abcdef.0123456789abcdef"}
		},
		"agent": {"nodeName": "test-node"},
		"node": {"kubelet": {"clusterFQDN": "cluster.example", "caCertData": "Y2E="}}
	}`
	var cfg config.Config
	if err := json.Unmarshal([]byte(baseline), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(overrides), &cfg); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestActivationTargetsUseCanonicalConfigAndAppliedMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		data      string
		applied   isolation.Mode
		wantAfter []string
		wantWants []string
	}{
		{
			name:      "applied none overrides desired nspawn with ARC",
			data:      `{"bootstrap":{"isolation":"nspawn"},"azure":{"arc":{"enabled":true}}}`,
			applied:   isolation.ModeNone,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
		{
			name:      "applied nspawn overrides desired none without ARC",
			data:      `{"bootstrap":{"isolation":"none"},"azure":{"arc":{"enabled":false}}}`,
			applied:   isolation.ModeNSpawn,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "machines.target"},
		},
		{
			name:      "default configuration selects nspawn targets",
			data:      `{}`,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "machines.target"},
		},
		{
			name:      "configured none and ARC without applied state",
			data:      `{"bootstrap":{"isolation":"none"},"azure":{"arc":{"enabled":true}}}`,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
		{
			name:      "applied OCI overrides desired nspawn and unit ARC",
			data:      `{"bootstrap":{"isolation":"nspawn"},"azure":{"arc":{"enabled":false}}}`,
			applied:   isolation.ModeContainer,
			unit:      "After=himdsd.service\nWants=machines.target\n",
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service"},
		},
		{
			name:      "configured OCI and ARC without applied state",
			data:      `{"bootstrap":{"isolation":"container","experimental":{"containerIsolation":true}},"azure":{"arc":{"enabled":true}}}`,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			data := writeActivationConfig(t, configPath, tc.data)
			var state *State
			if tc.applied != "" {
				state = &State{Isolation: tc.applied}
			}
			leaf := &activationTargetService{active: true}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{state: state},
				configPath:            configPath,
			}

			for range 2 {
				if _, err := service.Preflight(t.Context(), "/unused/current"); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(leaf.targets.AfterTargets, tc.wantAfter) || !slices.Equal(leaf.targets.WantsTargets, tc.wantWants) {
					t.Fatalf("targets = %+v, want After=%q Wants=%q", leaf.targets, tc.wantAfter, tc.wantWants)
				}
			}
			if leaf.prepares != 0 || leaf.restarts != 0 {
				t.Fatal("preflight prepared or restarted the service")
			}
			if got, err := os.ReadFile(configPath); err != nil || string(got) != data {
				t.Fatalf("preflight changed desired config: %v", err)
			}
		})
	}
}

func TestActivationRequiresValidConfig(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		state        *State
		data         string
		overrides    bool
		preflightErr error
		wantErr      string
		wantPlans    int
	}{
		{
			name:    "fully reset host requires config",
			wantErr: "load host agent configuration",
		},
		{
			name:    "applied state still requires config",
			state:   &State{Isolation: isolation.ModeNSpawn},
			wantErr: "load host agent configuration",
		},
		{
			name:         "unknown systemd state",
			data:         `{}`,
			overrides:    true,
			preflightErr: errors.New("systemd unavailable"),
			wantErr:      "systemd unavailable",
			wantPlans:    1,
		},
		{
			name:    "malformed config is not absent",
			data:    `{`,
			wantErr: "error unmarshaling config",
		},
		{
			name:      "unsupported desired mode without applied state",
			data:      `{"bootstrap":{"isolation":"invalid"}}`,
			overrides: true,
			wantErr:   "unsupported worker isolation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			if tc.overrides {
				writeActivationConfig(t, configPath, tc.data)
			} else if tc.data != "" {
				if err := os.WriteFile(configPath, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			leaf := &activationTargetService{preflightErr: tc.preflightErr}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{state: tc.state},
				configPath:            configPath,
			}

			_, err := service.Preflight(t.Context(), "/unused/current")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Preflight error = %v, want %q", err, tc.wantErr)
			}
			if leaf.preflights != tc.wantPlans || leaf.prepares != 0 || leaf.restarts != 0 {
				t.Fatalf("service calls: preflight=%d prepare=%d restart=%d", leaf.preflights, leaf.prepares, leaf.restarts)
			}
		})
	}
}

func TestBinaryActivation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		active     bool
		configured bool
		candidate  string
		wantErr    string
	}{
		{
			name:    "missing config is refused",
			wantErr: "load host agent configuration",
		},
		{
			name:       "configured inactive host stays inactive",
			configured: true,
		},
		{
			name:       "configured active host restarts",
			configured: true,
			active:     true,
		},
		{
			name:       "dangling candidate",
			configured: true,
			candidate:  "dangling",
			wantErr:    "open host agent activation candidate",
		},
		{
			name:       "candidate is a directory",
			configured: true,
			candidate:  "directory",
			wantErr:    "not a regular executable",
		},
		{
			name:       "candidate is not executable",
			configured: true,
			candidate:  "nonexecutable",
			wantErr:    "not a regular executable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			paths := agentdeployment.UpgradePathsUnder(dir)
			if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o750); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, paths.BluePath, "#!/bin/sh\nexit 0\n")
			for link, target := range map[string]string{
				paths.CurrentPath:  paths.BluePath,
				paths.LastGoodPath: paths.BluePath,
				paths.BinaryPath:   paths.CurrentPath,
			} {
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			candidate := filepath.Join(dir, "candidate")
			switch tc.candidate {
			case "dangling":
				if err := os.Symlink(filepath.Join(dir, "missing"), candidate); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(candidate, 0o750); err != nil {
					t.Fatal(err)
				}
			case "nonexecutable":
				if err := os.WriteFile(candidate, []byte("candidate"), 0o644); err != nil {
					t.Fatal(err)
				}
			default:
				writeExecutable(t, candidate, "#!/bin/sh\n[ \"$1\" = version ]\n")
			}

			configPath := filepath.Join(dir, "config.json")
			if tc.configured {
				writeActivationConfig(t, configPath, `{}`)
			}

			leaf := &activationTargetService{active: tc.active}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{},
				log:                   discardLogger(),
				configPath:            configPath,
				synchronize: func(string, string) error {
					t.Error("activation synchronized a missing worker")
					return nil
				},
			}

			result, err := agentbinary.ActivateHostDaemon(t.Context(), discardLogger(), agentbinary.ActivationOptions{
				Layout:        paths.Layout(),
				CandidatePath: candidate,
				LockPath:      filepath.Join(dir, "activation.lock"),
			}, service)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("activation error = %v, want %q", err, tc.wantErr)
				}
				if leaf.prepares != 0 || leaf.restarts != 0 {
					t.Fatal("refused activation prepared or restarted the service")
				}
				assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
				if _, err := os.Stat(paths.GreenPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused activation staged the candidate: %v", err)
				}
				return
			}
			if err != nil || result.RolledBack {
				t.Fatalf("activation result = %+v, error = %v", result, err)
			}

			wantRestarts := 0
			if tc.active {
				wantRestarts = 1
			}
			if leaf.prepares != 1 || leaf.restarts != wantRestarts {
				t.Fatalf("prepare calls = %d, restarts = %d; want 1 and %d", leaf.prepares, leaf.restarts, wantRestarts)
			}
			assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
		})
	}
}

func TestActivationConstructorUsesCanonicalPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	service, _, err := newFlexDaemonActivationService(discardLogger(), func() string { return root })
	if err != nil {
		t.Fatal(err)
	}
	if service.configPath != filepath.Join(config.ConfigDir, "config.json") {
		t.Fatalf("activation config path = %q", service.configPath)
	}
}
