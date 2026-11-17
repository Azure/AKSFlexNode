package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/config"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func activationFenceJournal(t *testing.T, scenario string) containerStartupStore {
	t.Helper()
	store := containerStartupStore{path: filepath.Join(t.TempDir(), "startup.json")}
	if scenario == "absent" {
		return store
	}
	if scenario == "unreadable" {
		// A directory fails ReadFile even when the tests run as root.
		if err := os.Mkdir(store.path, 0o700); err != nil {
			t.Fatal(err)
		}
		return store
	}
	if scenario == "corrupt" {
		writeTestFile(t, store.path, "{broken")
		return store
	}
	startup := &containerStartup{Phase: scenario, Previous: containerTestState("kube1", "old"), Next: containerTestState("kube2", "next")}
	switch scenario {
	case "bad checksum", "bad version", "invalid frozen state", "invalid phase":
		startup.Phase = "starting"
		if scenario == "invalid frozen state" {
			startup.Next.NodeConfig = nil
		}
		if scenario == "invalid phase" {
			startup.Phase = "unknown"
		}
		payload, err := json.Marshal(startup)
		if err != nil {
			t.Fatal(err)
		}
		record := noneStateRecord{Version: 1, Payload: payload, Checksum: checksum(payload)}
		if scenario == "bad checksum" {
			record.Checksum = "wrong"
		}
		if scenario == "bad version" {
			record.Version = 2
		}
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, store.path, string(data))
	default:
		if err := store.Save(t.Context(), startup); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func activationFenceService(t *testing.T, startup containerStartupStore, state *State) *flexDaemonActivationService {
	t.Helper()
	return &flexDaemonActivationService{
		log: discardLogger(), paths: testAgentUpgradePaths(t),
		provider: containerTestProvider(t, startup), state: &testStateStore{state: state},
		systemdDir: t.TempDir(), recoveryScript: filepath.Join(t.TempDir(), "recovery.sh"),
		inspectService: func(context.Context, *slog.Logger, string) (bool, error) { return false, nil },
	}
}

func TestAgentActivationFencesPendingContainerStartup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"preparing", "starting", "finished", "corrupt", "bad checksum", "bad version", "invalid frozen state", "invalid phase", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			for _, mode := range []isolation.Mode{isolation.ModeContainer, isolation.ModeNSpawn, isolation.ModeNone} {
				t.Run(string(mode), func(t *testing.T) {
					t.Parallel()
					startup := activationFenceJournal(t, scenario)
					state := &State{Isolation: mode, ActiveMachine: "kube1"}
					service := activationFenceService(t, startup, state)
					executor := &hostAgentUpgradeExecutor{
						log: discardLogger(), paths: service.paths, provider: service.provider,
						state: &fakeNodeOperator{state: state}, signals: agentUpgradeSignalStore{path: service.paths.SignalPath},
					}
					for range 2 {
						checks := []struct {
							name string
							run  func() error
						}{
							{"record pending", func() error { return executor.RecordPending(t.Context(), "upgrade") }},
							{"stage", func() error { return executor.Stage(t.Context(), agentUpgradeRequest{}) }},
							{"judge started upgrade", func() error { return executor.validateStarted(&agentUpgradeSignal{}) }},
							{"preflight", func() error { _, err := service.Preflight(t.Context(), service.paths.CurrentPath); return err }},
							{"prepare", func() error { return service.Prepare(t.Context(), service.paths.CurrentPath) }},
							{"synchronize", func() error { return service.synchronizeActiveNode(t.Context(), "/unused/candidate") }},
						}
						for _, check := range checks {
							if err := check.run(); err == nil || !strings.Contains(err.Error(), "container startup") {
								t.Fatalf("%s error = %v, want container startup fence", check.name, err)
							}
						}
					}
					for _, path := range []string{service.paths.CurrentPath, service.paths.LastGoodPath, service.paths.BluePath, service.paths.GreenPath, service.paths.SignalPath, service.recoveryScript, filepath.Join(service.systemdDir, ServiceUnitName)} {
						if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("activation mutated %s: %v", path, err)
						}
					}
				})
			}
		})
	}
}

func TestAgentActivationWithoutContainerStartup(t *testing.T) {
	t.Parallel()
	for _, mode := range []isolation.Mode{"", isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run("isolation="+string(mode), func(t *testing.T) {
			t.Parallel()
			startup := activationFenceJournal(t, "absent")
			state := &State{Isolation: mode, ActiveMachine: "kube1"}
			if mode == isolation.ModeNone {
				state.ActiveMachine = ""
			}
			service := activationFenceService(t, startup, state)
			if mode != isolation.ModeContainer {
				service.provider = mustNodeProvider(t, state.IsolationMode())
			}
			guardErr := errors.New("reached legacy guard")
			executor := &hostAgentUpgradeExecutor{
				state: &fakeNodeOperator{state: state}, provider: service.provider,
				signals: agentUpgradeSignalStore{path: service.paths.SignalPath}, guardLegacySeed: func() error { return guardErr },
			}
			if err := executor.RecordPending(t.Context(), "upgrade"); err != nil {
				t.Fatal(err)
			}
			signal, err := executor.signals.read()
			if err != nil || signal.ActiveMachine != state.ActiveMachine || signal.Isolation != state.IsolationMode() {
				t.Fatalf("signal = %+v, %v", signal, err)
			}
			if err := executor.Stage(t.Context(), agentUpgradeRequest{}); !errors.Is(err, guardErr) {
				t.Fatalf("Stage error = %v, want legacy guard", err)
			}
			if err := executor.signals.clear(); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Preflight(t.Context(), service.paths.CurrentPath); err != nil {
				t.Fatal(err)
			}
			if err := service.Prepare(t.Context(), service.paths.CurrentPath); err != nil {
				t.Fatal(err)
			}
			if mode == isolation.ModeNone {
				if err := service.synchronizeActiveNode(t.Context(), "/unused/none"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAgentActivationRechecksStartupAfterLoadingState(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"starting", "corrupt", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			startup := activationFenceJournal(t, "absent")
			loader := activationFencePublishingState{testStateStore: &testStateStore{state: containerTestState("kube1", "old")}, publish: func() {
				published := activationFenceJournal(t, scenario)
				if err := os.Rename(published.path, startup.path); err != nil {
					t.Fatal(err)
				}
			}}
			if state, err := loadAgentActivationState(t.Context(), &operatorBase{state: loader}, containerTestProvider(t, startup)); err == nil || state != nil {
				t.Fatalf("LoadState accepted a newly published startup: %+v, %v", state, err)
			}
		})
	}
}

// Only the lock path is replaced; admission, staging and recovery use the host executor.
// Each test owns its lock rather than contending with other tests or the host daemon.
type activationFenceUpgrade struct {
	*hostAgentUpgradeExecutor
	lockPath string
}

func (e activationFenceUpgrade) Acquire() (io.Closer, error) {
	return agentbinary.AcquireHostActivationLock(e.lockPath)
}

func activationFenceOperation() agentdaemon.MachineOperation {
	return agentdaemon.MachineOperation{Name: "upgrade", Kind: machinav1alpha3.OperationAgentUpgrade, Parameters: map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.test/agent.tar.gz", agentUpgradeSHA256Parameter: strings.Repeat("a", 64),
	}}
}

func TestAgentActivationRejectsStoppedSourceDuringContainerRepave(t *testing.T) {
	t.Parallel()
	op, events, store := containerOperatorFixture(t, "npd", false)
	if _, err := op.ApplyGoalState(t.Context(), discardLogger(), aksmachine.GoalState{SettingsVersion: "next", KubernetesVersion: "1.36.2"}); err == nil {
		t.Fatal("expected NPD failure after target startup")
	}
	startup, err := op.startup.Load()
	if err != nil || startup == nil || startup.Phase != "starting" || startup.Next.ActiveMachine != "kube2" || store.state.ActiveMachine != "kube1" {
		t.Fatalf("startup = %+v, applied = %+v, error = %v", startup, store.state, err)
	}
	if !slices.Contains(*events, "source-stop") || !slices.Contains(*events, "start") {
		t.Fatalf("did not reproduce stopped source and running target: %v", *events)
	}
	service := activationFenceService(t, op.startup, store.state)
	executor := &hostAgentUpgradeExecutor{
		log: discardLogger(), state: op, provider: service.provider, paths: service.paths,
		signals: agentUpgradeSignalStore{path: service.paths.SignalPath},
	}
	handler := machineOperationHandlers{log: discardLogger(), agentUpgrade: activationFenceUpgrade{executor, filepath.Join(t.TempDir(), "lock")}}
	for range 2 {
		operations := &fakeMachineOperationStore{}
		if _, err := handler.reconcileAgentUpgrade(t.Context(), operations, activationFenceOperation()); err != nil {
			t.Fatal(err)
		}
		if operations.inProgress || operations.result.Phase != machinav1alpha3.OperationPhaseFailed || !strings.Contains(operations.result.Message, "container startup") {
			t.Fatalf("unsafe MachineOperation admission: %+v", operations)
		}
		if _, err := service.Preflight(t.Context(), service.paths.CurrentPath); err == nil {
			t.Fatal("direct activation accepted the stopped source")
		}
		if signal, err := executor.signals.read(); err != nil || signal != nil {
			t.Fatalf("denied upgrade recorded a signal: %+v, %v", signal, err)
		}
	}
	op.startNPD = func(context.Context, *slog.Logger, *config.Config, *goalstates.NodeStart) error { return nil }
	if handled, err := op.ResumeRepave(t.Context(), discardLogger(), nil); !handled || err != nil {
		t.Fatalf("resume = %t, %v", handled, err)
	}
	if err := executor.RecordPending(t.Context(), "upgrade"); err != nil {
		t.Fatal(err)
	}
	if signal, err := executor.signals.read(); err != nil || signal.ActiveMachine != "kube2" {
		t.Fatalf("post-recovery upgrade did not select target: %+v, %v", signal, err)
	}
}

func TestPendingAgentUpgradeRecoveryBypassesContainerStartupFence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, scenario, failure string
		restarts                int
	}{
		{name: "preparing recovery", scenario: "preparing", failure: "candidate failed", restarts: 1},
		{name: "starting recovery", scenario: "starting", failure: "candidate failed", restarts: 1},
		{name: "finished recovery", scenario: "finished", failure: "candidate failed", restarts: 1},
		{name: "corrupt recovery", scenario: "corrupt", failure: "candidate failed", restarts: 1},
		{name: "unreadable recovery", scenario: "unreadable", failure: "candidate failed", restarts: 1},
		{name: "pending startup fails candidate judgment without deadlock", scenario: "starting"},
		{name: "corrupt startup fails candidate judgment without deadlock", scenario: "corrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			startup := activationFenceJournal(t, tc.scenario)
			paths := testAgentUpgradePaths(t)
			writeTestFile(t, paths.BluePath, "last-good")
			writeTestFile(t, paths.GreenPath, "candidate")
			for link, target := range map[string]string{paths.CurrentPath: paths.GreenPath, paths.LastGoodPath: paths.BluePath} {
				if err := os.Symlink(target, link); err != nil {
					t.Fatal(err)
				}
			}
			signals := agentUpgradeSignalStore{path: paths.SignalPath}
			if err := signals.write(agentUpgradeSignal{OperationName: "upgrade", Isolation: isolation.ModeNone, CandidatePath: paths.GreenPath, SwitchCommitted: true,
				InitiatingDaemonInstance: "initiator", Failure: tc.failure, RecoveryRequired: tc.failure != ""}); err != nil {
				t.Fatal(err)
			}
			restarts, finished := 0, 0
			executor := &hostAgentUpgradeExecutor{
				log: discardLogger(), paths: paths, provider: containerTestProvider(t, startup), signals: signals, instanceID: "recovery",
				state:             &fakeNodeOperator{err: errors.New("recovery must not load applied state")},
				systemctl:         func(context.Context, ...string) error { return nil },
				runSystemdRun:     func(context.Context, ...string) error { restarts++; return nil },
				runningExecutable: func() (string, error) { return paths.BluePath, nil },
				finishMachineOperation: func(_ context.Context, _ client.Client, _ agentdaemon.MachineOperation, result agentdaemon.MachineOperationResult[int64]) error {
					if result.Phase != machinav1alpha3.OperationPhaseFailed {
						t.Fatalf("recovery phase = %s", result.Phase)
					}
					finished++
					return nil
				},
			}
			handler := machineOperationHandlers{log: discardLogger(), agentUpgrade: activationFenceUpgrade{executor, filepath.Join(t.TempDir(), "lock")}}
			operations := &fakeMachineOperationStore{}
			if _, err := handler.reconcileAgentUpgrade(t.Context(), operations, activationFenceOperation()); err != nil {
				t.Fatal(err)
			}
			if restarts != tc.restarts || operations.inProgress || operations.result.Phase != "" {
				t.Fatalf("recovery blocked or restaged: restarts=%d, operations=%+v", restarts, operations)
			}
			for range 2 {
				if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), nil, executor); err != nil {
					t.Fatal(err)
				}
			}
			assertResolvedPath(t, paths.CurrentPath, paths.BluePath)
			if signal, err := signals.read(); err != nil || signal != nil || finished != 1 {
				t.Fatalf("recovery did not complete idempotently: %+v, %v, finished=%d", signal, err, finished)
			}
			if err := requireNoContainerStartupForAgentActivation(startup); err == nil {
				t.Fatal("AgentUpgrade recovery removed the container startup fence")
			}
		})
	}
}

func TestAgentActivationDoesNotValidateMatchingStoppedSource(t *testing.T) {
	t.Parallel()
	paths := testAgentUpgradePaths(t)
	writeTestFile(t, paths.BluePath, "candidate")
	helper := writeTestFile(t, filepath.Join(t.TempDir(), "stopped-source-helper"), "candidate")
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	signal := &agentUpgradeSignal{OperationName: "upgrade", Isolation: isolation.ModeContainer, ActiveMachine: "kube1", CandidatePath: paths.BluePath}
	helperPath := func(string) string { return helper }
	provider := containerTestProvider(t, activationFenceJournal(t, "absent"))
	provider.binaryPath = helperPath
	if err := validateStartedAgentUpgrade(paths, signal, provider); err != nil {
		t.Fatalf("expected matching source to pass binary checks: %v", err)
	}
	provider.startup = activationFenceJournal(t, "starting")
	executor := &hostAgentUpgradeExecutor{paths: paths, provider: provider}
	if err := executor.validateStarted(signal); err == nil || !strings.Contains(err.Error(), "container startup") {
		t.Fatalf("stopped-source upgrade judged successful: %v", err)
	}
}

func TestRecordPendingAgentUpgradeRejectsMissingActiveNode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state *State
	}{
		{name: "missing state"},
		{name: "default without slot", state: &State{}},
		{name: "default invalid slot", state: &State{ActiveMachine: "../escape"}},
		{name: "nspawn without slot", state: &State{Isolation: isolation.ModeNSpawn}},
		{name: "nspawn invalid slot", state: &State{Isolation: isolation.ModeNSpawn, ActiveMachine: "../escape"}},
		{name: "container without slot", state: &State{Isolation: isolation.ModeContainer}},
		{name: "container invalid slot", state: &State{Isolation: isolation.ModeContainer, ActiveMachine: "../escape"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			signals := agentUpgradeSignalStore{path: filepath.Join(t.TempDir(), "upgrade.json")}
			for range 2 {
				mode := isolation.ModeNSpawn
				if tc.state != nil {
					mode = tc.state.IsolationMode()
				}
				provider := mustNodeProvider(t, mode)
				if mode == isolation.ModeContainer {
					provider = containerTestProvider(t, activationFenceJournal(t, "absent"))
				}
				err := recordPendingAgentUpgrade(t.Context(), signals, &fakeNodeOperator{state: tc.state}, "upgrade", "daemon", provider)
				if err == nil || !strings.Contains(err.Error(), "no valid active node for AgentUpgrade") {
					t.Fatalf("RecordPending error = %v, want no valid active node", err)
				}
				if signal, err := signals.read(); err != nil || signal != nil {
					t.Fatalf("rejected upgrade recorded signal: %+v, %v", signal, err)
				}
			}
		})
	}
}

func TestDirectAgentActivationRequiresLogger(t *testing.T) {
	t.Parallel()
	if _, _, err := newFlexDaemonActivationService(nil, func() string { t.Fatal("resolved root without logger"); return "" }, mustNodeProvider(t, "nspawn"), &testStateStore{}); err == nil {
		t.Fatal("accepted nil direct activation logger")
	}
}
