package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// Keep the shared activator's file mutations and Flex's admission/synchronization,
// replacing only systemd and the timing of a concurrently published startup.
type activationFenceDirectService struct {
	*flexDaemonActivationService
	beforePrepare func()
	calls         []string
}

func (s *activationFenceDirectService) Prepare(ctx context.Context, current string) error {
	s.calls = append(s.calls, "prepare")
	if s.beforePrepare != nil {
		s.beforePrepare()
	}
	return s.flexDaemonActivationService.Prepare(ctx, current)
}

func (s *activationFenceDirectService) Reload(context.Context) error {
	s.calls = append(s.calls, "reload")
	return nil
}

func (s *activationFenceDirectService) Restart(context.Context) error {
	s.calls = append(s.calls, "restart")
	return nil
}

func (s *activationFenceDirectService) WaitHealthy(ctx context.Context, expected string) error {
	s.calls = append(s.calls, "healthy")
	return s.flexDaemonActivationService.WaitHealthy(ctx, expected)
}

type activationFencePublishingState struct {
	*testStateStore
	publish func()
}

func (s activationFencePublishingState) Load(ctx context.Context) (*State, error) {
	s.publish()
	return s.testStateStore.Load(ctx)
}

func TestDirectAgentActivationRechecksBeforeSynchronization(t *testing.T) {
	t.Parallel()
	startup := activationFenceJournal(t, "absent")
	service := activationFenceService(t, startup, nil)
	service.state = activationFencePublishingState{
		testStateStore: &testStateStore{state: &State{Isolation: isolation.ModeNone}},
		publish: func() {
			if err := startup.Save(t.Context(), &containerStartup{Phase: "starting", Previous: containerTestState("kube1", "old"), Next: containerTestState("kube2", "next")}); err != nil {
				t.Fatal(err)
			}
		},
	}
	if err := service.synchronizeActiveNode(t.Context(), "/unused/none"); err == nil || !strings.Contains(err.Error(), "container startup") {
		t.Fatalf("startup published during state load was ignored: %v", err)
	}
}

func TestDirectAgentActivationFencesBeforeHostSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		before    bool
		during    bool
		wantCalls []string
	}{
		{name: "pending before preflight", before: true},
		{name: "pending after preflight", during: true, wantCalls: []string{"prepare"}},
		{name: "no pending startup", wantCalls: []string{"prepare", "reload", "restart", "healthy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			startup := activationFenceJournal(t, "absent")
			base := activationFenceService(t, startup, &State{Isolation: isolation.ModeNone})
			paths := base.paths
			const previous = "#!/bin/sh\nexit 0\n# previous\n"
			const candidate = "#!/bin/sh\nexit 0\n# candidate\n"
			writeTestFile(t, paths.BluePath, previous)
			writeTestFile(t, paths.GreenPath, "inactive")
			if err := os.MkdirAll(filepath.Dir(paths.BinaryPath), 0o750); err != nil {
				t.Fatal(err)
			}
			for link, target := range map[string]string{paths.CurrentPath: paths.BluePath, paths.LastGoodPath: paths.BluePath, paths.BinaryPath: paths.CurrentPath} {
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			publish := func() {
				if err := startup.Save(t.Context(), &containerStartup{Phase: "starting", Previous: containerTestState("kube1", "old"), Next: containerTestState("kube2", "next")}); err != nil {
					t.Fatal(err)
				}
			}
			service := &activationFenceDirectService{flexDaemonActivationService: base}
			if tc.before {
				publish()
			}
			if tc.during {
				service.beforePrepare = publish
			}
			opts := hostAgentActivationOptions(paths, writeTestFile(t, filepath.Join(t.TempDir(), "candidate"), candidate))
			opts.LockPath = filepath.Join(t.TempDir(), "lock")
			result, err := agentbinary.ActivateHostDaemon(t.Context(), discardLogger(), opts, service)
			wantDenied := tc.before || tc.during
			if wantDenied {
				if err == nil || !strings.Contains(err.Error(), "container startup") {
					t.Fatalf("ActivateHostDaemon = %+v, %v, want startup fence", result, err)
				}
				assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
				assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
				assertAgentUpgradeFileContent(t, paths.BluePath, previous)
				if tc.before {
					assertAgentUpgradeFileContent(t, paths.GreenPath, "inactive")
				}
			} else {
				if err != nil || !result.Changed {
					t.Fatalf("ActivateHostDaemon = %+v, %v", result, err)
				}
				assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
				assertResolvedPath(t, paths.LastGoodPath, paths.BluePath)
				assertAgentUpgradeFileContent(t, paths.GreenPath, candidate)
			}
			if !slices.Equal(service.calls, tc.wantCalls) {
				t.Fatalf("service calls = %v, want %v", service.calls, tc.wantCalls)
			}
			service.calls = nil
			result, err = agentbinary.ActivateHostDaemon(t.Context(), discardLogger(), opts, service)
			if wantDenied {
				if err == nil || len(service.calls) != 0 {
					t.Fatalf("retry bypassed startup fence: %+v, %v, calls=%v", result, err, service.calls)
				}
				assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
			} else if err != nil || result.Changed {
				t.Fatalf("activation was not idempotent: %+v, %v", result, err)
			}
		})
	}
}
