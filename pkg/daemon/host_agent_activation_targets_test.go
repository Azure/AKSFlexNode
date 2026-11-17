package daemon

import (
	"context"
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

func (s *activationTargetService) WasActive() bool { return s.active }

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

func TestActivationTargetsUseCanonicalConfigAndAppliedMode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		data      string
		applied   isolation.Mode
		unit      string
		wantAfter []string
		wantWants []string
	}{
		{
			name:      "applied none overrides desired nspawn",
			data:      `{"bootstrap":{"isolation":"nspawn"},"azure":{"arc":{"enabled":true}}}`,
			applied:   isolation.ModeNone,
			unit:      "Wants=machines.target\n",
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
		{
			name:      "applied nspawn overrides desired none and unit ARC",
			data:      `{"bootstrap":{"isolation":"none"},"azure":{"arc":{"enabled":false}}}`,
			applied:   isolation.ModeNSpawn,
			unit:      "After=himdsd.service\n",
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "machines.target"},
		},
		{
			name:      "default configuration ignores unit profile",
			data:      `{}`,
			unit:      "After=himdsd.service\n",
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "machines.target"},
		},
		{
			name:      "configured none and ARC without installed unit",
			data:      `{"bootstrap":{"isolation":"none"},"azure":{"arc":{"enabled":true}}}`,
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
		{
			name:      "binary repair ignores broken runtime and credential settings",
			data:      `{"bootstrap":{"isolation":"invalid"},"agent":{"logLevel":"invalid"},"azure":{"arc":{"enabled":true},"servicePrincipal":{"clientSecretFile":"missing-credential"}}}`,
			applied:   isolation.ModeNone,
			unit:      "Wants=machines.target\n",
			wantAfter: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
			wantWants: []string{"network-online.target", "systemd-udev-settle.service", "himdsd.service"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(configPath, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			unitPath := filepath.Join(dir, agentdeployment.ServiceUnitName)
			if tc.unit != "" {
				if err := os.WriteFile(unitPath, []byte(tc.unit), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			candidate := filepath.Join(dir, "candidate")
			writeExecutable(t, candidate, "#!/bin/sh\n[ \"$1\" = supports-isolation ] && [ \"$2\" = none ]\n")
			var state *State
			if tc.applied != "" {
				state = &State{Isolation: tc.applied}
			}
			leaf := &activationTargetService{active: true}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{state: state},
				candidatePath:         candidate,
				configPath:            configPath,
				unitPath:              unitPath,
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
			if got, err := os.ReadFile(configPath); err != nil || string(got) != tc.data {
				t.Fatalf("preflight changed desired config: %v", err)
			}
			got, err := os.ReadFile(unitPath)
			if tc.unit == "" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preflight created unit: %v", err)
				}
			} else if err != nil || string(got) != tc.unit {
				t.Fatalf("preflight changed installed unit: %v", err)
			}
		})
	}
}

func TestActivationConfigAbsenceRequiresInactiveResetHost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name         string
		state        *State
		unit         bool
		danglingUnit bool
		active       bool
		data         string
		preflightErr error
		wantErr      string
		wantPlans    int
	}{
		{
			name:      "fully reset host",
			wantPlans: 1,
		},
		{
			name:    "stopped installed unit still requires config",
			unit:    true,
			wantErr: "installed host agent requires configuration",
		},
		{
			name:         "dangling installed unit still requires config",
			danglingUnit: true,
			wantErr:      "installed host agent requires configuration",
		},
		{
			name:    "applied state still requires config",
			state:   &State{Isolation: isolation.ModeNSpawn},
			wantErr: "read host agent configuration",
		},
		{
			name:      "active service without unit still requires config",
			active:    true,
			wantErr:   "active host agent requires configuration",
			wantPlans: 1,
		},
		{
			name:         "unknown systemd state is not reset",
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
			name:    "unsupported desired mode without applied state",
			data:    `{"bootstrap":{"isolation":"invalid"}}`,
			wantErr: "unsupported worker isolation",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			if tc.data != "" {
				if err := os.WriteFile(configPath, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			unitPath := filepath.Join(dir, agentdeployment.ServiceUnitName)
			const unit = "After=himdsd.service\n"
			if tc.unit {
				if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.danglingUnit {
				if err := os.Symlink(filepath.Join(dir, "missing-unit"), unitPath); err != nil {
					t.Fatal(err)
				}
			}
			leaf := &activationTargetService{active: tc.active, preflightErr: tc.preflightErr}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{state: tc.state},
				configPath:            configPath,
				unitPath:              unitPath,
			}

			_, err := service.Preflight(t.Context(), "/unused/current")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				wantAfter := []string{"network-online.target", "systemd-udev-settle.service"}
				wantWants := []string{"network-online.target", "systemd-udev-settle.service", "machines.target"}
				if !slices.Equal(leaf.targets.AfterTargets, wantAfter) || !slices.Equal(leaf.targets.WantsTargets, wantWants) {
					t.Fatalf("reset targets = %+v", leaf.targets)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Preflight error = %v, want %q", err, tc.wantErr)
			}
			if leaf.preflights != tc.wantPlans || leaf.prepares != 0 || leaf.restarts != 0 {
				t.Fatalf("service calls: preflight=%d prepare=%d restart=%d", leaf.preflights, leaf.prepares, leaf.restarts)
			}
			if tc.unit {
				if got, err := os.ReadFile(unitPath); err != nil || string(got) != unit {
					t.Fatalf("installed unit changed: %v", err)
				}
			}
		})
	}
}

func TestConfiglessBinaryActivation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		active bool
		unit   bool
	}{
		{name: "reset host stays inactive"},
		{name: "active host is refused", active: true},
		{name: "installed host is refused", unit: true},
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
			writeExecutable(t, candidate, "#!/bin/sh\n[ \"$1\" = version ]\n")
			unitPath := filepath.Join(dir, agentdeployment.ServiceUnitName)
			const unit = "After=himdsd.service\n"
			if tc.unit {
				if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			leaf := &activationTargetService{active: tc.active}
			service := &flexDaemonActivationService{
				hostActivationService: leaf,
				state:                 &testStateStore{},
				log:                   discardLogger(),
				candidatePath:         candidate,
				configPath:            filepath.Join(dir, "config.json"),
				unitPath:              unitPath,
				synchronize: func(string, string) error {
					t.Error("configless host synchronized a missing worker")
					return nil
				},
			}

			result, err := agentbinary.ActivateHostDaemon(t.Context(), discardLogger(), agentbinary.ActivationOptions{
				Layout:        paths.Layout(),
				CandidatePath: candidate,
				LockPath:      filepath.Join(dir, "activation.lock"),
			}, service)
			if tc.active || tc.unit {
				if err == nil || !strings.Contains(err.Error(), "requires configuration") {
					t.Fatalf("activation error = %v, want missing configuration", err)
				}
				if leaf.prepares != 0 || leaf.restarts != 0 {
					t.Fatal("refused activation prepared or restarted the service")
				}
				assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
				if _, err := os.Stat(paths.GreenPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("refused activation staged the candidate: %v", err)
				}
				if tc.unit {
					if got, err := os.ReadFile(unitPath); err != nil || string(got) != unit {
						t.Fatalf("refused activation changed unit: %v", err)
					}
				}
				return
			}
			if err != nil || result.RolledBack {
				t.Fatalf("activation result = %+v, error = %v", result, err)
			}
			if leaf.prepares != 1 || leaf.restarts != 0 {
				t.Fatalf("prepare calls = %d, restarts = %d; want 1 and 0", leaf.prepares, leaf.restarts)
			}
			assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
		})
	}
}

func TestActivationConstructorUsesCanonicalPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	service, _, err := newFlexDaemonActivationService(discardLogger(), func() string { return root }, "/unused/candidate")
	if err != nil {
		t.Fatal(err)
	}
	if service.configPath != filepath.Join(config.ConfigDir, "config.json") || service.unitPath != agentdeployment.ServiceUnitPath {
		t.Fatalf("activation paths = %q, %q", service.configPath, service.unitPath)
	}
}
