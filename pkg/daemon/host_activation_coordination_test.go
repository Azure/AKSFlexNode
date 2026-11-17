package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

type activationStateFixture struct {
	states       []*State
	loads        int
	wantDeadline time.Time
}

func (s *activationStateFixture) Load(ctx context.Context) (*State, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("activation health deadline missing")
	}
	if !s.wantDeadline.IsZero() && !deadline.Equal(s.wantDeadline) {
		return nil, fmt.Errorf("state deadline = %s, want %s", deadline, s.wantDeadline)
	}
	if s.loads >= len(s.states) {
		return nil, errors.New("unexpected state load")
	}
	state := s.states[s.loads]
	s.loads++
	return state, nil
}
func (*activationStateFixture) Save(context.Context, *State) error { return nil }
func (*activationStateFixture) Delete(context.Context) error       { return nil }

type testActivationService struct {
	coordinator *flexDaemonActivationService
	restarts    int
}

func (*testActivationService) Preflight(context.Context, string) (agentbinary.ServicePlan, error) {
	return agentbinary.ServicePlan{}, nil
}
func (*testActivationService) Prepare(context.Context, string) error { return nil }
func (*testActivationService) Reload(context.Context) error          { return nil }
func (s *testActivationService) Restart(context.Context) error {
	s.restarts++
	return nil
}
func (s *testActivationService) WaitHealthy(ctx context.Context, expected string) error {
	return s.coordinator.WaitHealthy(ctx, expected)
}

func TestInactiveActivationSkipsMissingWorker(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "candidate")
	writeExecutable(t, binary, "candidate")
	store := &activationStateFixture{states: []*State{nil}}
	coordinator := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		state:                 store,
		log:                   slog.New(slog.DiscardHandler),
		synchronize: func(string, string) error {
			t.Error("synchronized a missing worker")
			return nil
		},
	}
	if err := coordinator.WaitHealthy(t.Context(), binary); err != nil {
		t.Fatalf("inactive activation: %v", err)
	}
	if store.loads != 1 {
		t.Fatalf("state loads = %d, want 1", store.loads)
	}
}

func TestNoneActivationSkipsGuestSynchronization(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "candidate")
	writeExecutable(t, binary, "candidate")
	store := &activationStateFixture{states: []*State{
		{
			Isolation:     isolation.ModeNone,
			ActiveMachine: "kube1",
		},
	}}
	coordinator := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		state:                 store,
		log:                   slog.New(slog.DiscardHandler),
		synchronize: func(string, string) error {
			t.Error("none activation synchronized a guest helper")
			return nil
		},
	}
	if err := coordinator.WaitHealthy(t.Context(), binary); err != nil {
		t.Fatalf("none activation: %v", err)
	}
	if store.loads != 1 {
		t.Fatalf("state loads = %d, want 1", store.loads)
	}
}

func TestActivationHealthFailureDoesNotLoadState(t *testing.T) {
	t.Parallel()

	store := &activationStateFixture{}
	coordinator := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		state:                 store,
		log:                   slog.New(slog.DiscardHandler),
		synchronize: func(string, string) error {
			t.Error("synchronized after health failure")
			return nil
		},
	}
	if err := coordinator.WaitHealthy(t.Context(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("health accepted a missing binary")
	}
	if store.loads != 0 {
		t.Fatalf("state loaded %d times after health failure", store.loads)
	}
}

func TestActivationPreservesEarlierHealthDeadline(t *testing.T) {
	t.Parallel()

	binary := filepath.Join(t.TempDir(), "candidate")
	writeExecutable(t, binary, "candidate")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()
	store := &activationStateFixture{
		states:       []*State{nil},
		wantDeadline: deadline,
	}
	coordinator := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		state:                 store,
		log:                   slog.New(slog.DiscardHandler),
	}
	if err := coordinator.WaitHealthy(ctx, binary); err != nil {
		t.Fatalf("health with earlier deadline: %v", err)
	}
	if store.loads != 1 {
		t.Fatalf("state loads = %d, want 1", store.loads)
	}
}

func TestActivationWorkerSyncFailureRollsBackAndReloadsState(t *testing.T) {
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

	store := &activationStateFixture{states: []*State{
		{
			Isolation:     isolation.ModeNSpawn,
			ActiveMachine: "kube1",
		},
		{
			Isolation:     isolation.ModeNSpawn,
			ActiveMachine: "kube2",
		},
	}}
	var copies []string
	syncErr := errors.New("worker copy failed")
	coordinator := &flexDaemonActivationService{
		HostActivationService: &agentdeployment.HostActivationService{},
		state:                 store,
		log:                   slog.New(slog.DiscardHandler),
		synchronize: func(path, destination string) error {
			copies = append(copies, fmt.Sprintf("%s:%s", filepath.Base(path), destination))
			if strings.Contains(destination, "/kube1/") {
				return syncErr
			}
			return nil
		},
	}
	service := &testActivationService{coordinator: coordinator}
	result, err := agentbinary.ActivateHostDaemon(t.Context(), slog.New(slog.DiscardHandler), agentbinary.ActivationOptions{
		Layout:        paths.Layout(),
		CandidatePath: candidate,
		LockPath:      filepath.Join(dir, "activation.lock"),
	}, service)
	if err == nil || !strings.Contains(err.Error(), "worker copy failed") || !result.RolledBack {
		t.Fatalf("activation result = %+v, error = %v", result, err)
	}
	if service.restarts != 2 || store.loads != 2 {
		t.Fatalf("restarts = %d, state loads = %d; want 2 each", service.restarts, store.loads)
	}
	want := []string{
		filepath.Base(paths.GreenPath) + ":/var/lib/machines/kube1/usr/local/bin/aks-flex-node",
		filepath.Base(paths.BluePath) + ":/var/lib/machines/kube2/usr/local/bin/aks-flex-node",
	}
	if !slices.Equal(copies, want) {
		t.Fatalf("worker copies = %v, want %v", copies, want)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
}
