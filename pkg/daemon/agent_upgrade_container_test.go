package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const (
	testAgentImageV1 = "registry.example/aks-flex-node-agent:v1"
	testAgentImageV2 = "registry.example/aks-flex-node-agent:v2"
)

// newTestContainerUpgrade returns a container executor with its files in a test directory, and
// the ctr and systemd-run commands it runs, which are recorded.
func newTestContainerUpgrade(t *testing.T, runningImage string) (*containerAgentUpgradeExecutor, *[]string) {
	t.Helper()
	dir := t.TempDir()
	calls := &[]string{}
	record := func(command string) func(context.Context, ...string) error {
		return func(_ context.Context, args ...string) error {
			*calls = append(*calls, command+" "+strings.Join(args, " "))
			return nil
		}
	}
	return &containerAgentUpgradeExecutor{
		log:           discardLogger(),
		state:         &fakeNodeOperator{state: &State{ActiveMachine: "kube1"}},
		signals:       agentUpgradeSignalStore{path: filepath.Join(dir, "agent-upgrade-signal.json")},
		images:        agentImageSelection{path: filepath.Join(dir, "agent-image.env")},
		runningImage:  runningImage,
		pullArgs:      []string{"--plain-http"},
		runArgs:       []string{"--snapshotter", "native"},
		ctr:           record("ctr"),
		runSystemdRun: record("systemd-run"),
		instanceID:    "instance",
	}, calls
}

func writeTestFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func assertSelectedImages(t *testing.T, images agentImageSelection, wantCurrent, wantLastGood string) {
	t.Helper()
	current, lastGood, err := images.read()
	if err != nil {
		t.Fatalf("read the image selection: %v", err)
	}
	if current != wantCurrent || lastGood != wantLastGood {
		t.Errorf("selected images = %q, last good %q; want %q, last good %q", current, lastGood, wantCurrent, wantLastGood)
	}
}

func TestAgentUpgradeExecutorsTakeTheirOwnKindOfRequest(t *testing.T) {
	t.Parallel()

	image := agentUpgradeRequest{image: testAgentImageV2}
	archive := agentUpgradeRequest{downloadURL: "https://example.com/agent.tar.gz"}
	tests := map[string]struct {
		executor agentUpgradeExecutor
		request  agentUpgradeRequest
		wantErr  bool
	}{
		"a host takes an archive":      {executor: &hostAgentUpgradeExecutor{}, request: archive},
		"a host refuses an image":      {executor: &hostAgentUpgradeExecutor{}, request: image, wantErr: true},
		"a container takes an image":   {executor: &containerAgentUpgradeExecutor{}, request: image},
		"a container refuses archives": {executor: &containerAgentUpgradeExecutor{}, request: archive, wantErr: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := tc.executor.Validate(tc.request); (err != nil) != tc.wantErr {
				t.Errorf("Validate() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestContainerAgentUpgradeStage(t *testing.T) {
	t.Parallel()

	pull := "ctr --namespace aks-flex image pull --plain-http " + testAgentImageV2
	check := "ctr --namespace aks-flex run --rm --snapshotter native " + testAgentImageV2 + " aks-flex-node-upgrade-check-"
	tests := map[string]struct {
		runningImage   string
		selected       string
		failOn         string
		wantCalls      []string
		wantErr        bool
		wantPrevious   string
		wantCommitted  bool
		wantSelected   string
		wantLastGood   string
		wantNoSelected bool
	}{
		"switches from the image the host's provisioning names": {
			runningImage:  testAgentImageV1,
			wantCalls:     []string{pull, check},
			wantPrevious:  testAgentImageV1,
			wantCommitted: true,
			wantSelected:  testAgentImageV2,
			wantLastGood:  testAgentImageV1,
		},
		"switches from the image an earlier upgrade selected": {
			selected:      testAgentImageV1,
			wantCalls:     []string{pull, check},
			wantPrevious:  testAgentImageV1,
			wantCommitted: true,
			wantSelected:  testAgentImageV2,
			wantLastGood:  testAgentImageV1,
		},
		"doesn't select an image it couldn't pull": {
			runningImage:   testAgentImageV1,
			failOn:         "pull",
			wantCalls:      []string{pull},
			wantErr:        true,
			wantPrevious:   testAgentImageV1,
			wantNoSelected: true,
		},
		"doesn't select an image whose agent doesn't run": {
			runningImage:   testAgentImageV1,
			failOn:         "run",
			wantCalls:      []string{pull, check},
			wantErr:        true,
			wantPrevious:   testAgentImageV1,
			wantNoSelected: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			executor, calls := newTestContainerUpgrade(t, tc.runningImage)
			executor.ctr = func(_ context.Context, args ...string) error {
				*calls = append(*calls, "ctr "+strings.Join(args, " "))
				if slices.Contains(args, tc.failOn) {
					return os.ErrPermission
				}
				return nil
			}
			if tc.selected != "" {
				if err := executor.images.write(tc.selected, tc.selected); err != nil {
					t.Fatal(err)
				}
			}
			if err := executor.RecordPending(t.Context(), "upgrade"); err != nil {
				t.Fatalf("RecordPending: %v", err)
			}

			err := executor.Stage(t.Context(), agentUpgradeRequest{image: testAgentImageV2})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Stage() error = %v, want error %v", err, tc.wantErr)
			}
			if len(*calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", *calls, tc.wantCalls)
			}
			for i, want := range tc.wantCalls {
				if !strings.HasPrefix((*calls)[i], want) {
					t.Errorf("call %d = %q, want it to start with %q", i, (*calls)[i], want)
				}
			}
			if len(*calls) == 2 && !strings.HasSuffix((*calls)[1], " aks-flex-node version") {
				t.Errorf("the check runs %q, not the candidate's aks-flex-node version", (*calls)[1])
			}
			signal, err := executor.signals.read()
			if err != nil || signal == nil {
				t.Fatalf("signal = %#v, %v", signal, err)
			}
			if signal.CandidateImage != testAgentImageV2 || signal.PreviousImage != tc.wantPrevious ||
				signal.SwitchCommitted != tc.wantCommitted {
				t.Errorf("signal = %#v", signal)
			}
			if tc.wantNoSelected {
				assertSelectedImages(t, executor.images, tc.selected, tc.selected)
			} else {
				assertSelectedImages(t, executor.images, tc.wantSelected, tc.wantLastGood)
			}
		})
	}
}

func TestContainerAgentUpgradeAbortRestoresThePreviousImage(t *testing.T) {
	t.Parallel()

	executor, _ := newTestContainerUpgrade(t, testAgentImageV1)
	if err := executor.RecordPending(t.Context(), "upgrade"); err != nil {
		t.Fatalf("RecordPending: %v", err)
	}
	if err := executor.Stage(t.Context(), agentUpgradeRequest{image: testAgentImageV2}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	// As when the restart can't be scheduled after the switch.
	if err := executor.Abort(t.Context()); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	assertSelectedImages(t, executor.images, testAgentImageV1, testAgentImageV1)
	if signal, err := executor.signals.read(); err != nil || signal != nil {
		t.Errorf("signal after Abort = %#v, %v", signal, err)
	}
}

// startedContainerUpgrade sets up a daemon starting after an upgrade's restart: the signal of a
// committed switch from v1 to v2, and the agent binary this daemon runs.
func startedContainerUpgrade(t *testing.T, runningImage, machineBinary string) (*containerAgentUpgradeExecutor, *[]string, *[]machinav1alpha3.OperationPhase) {
	t.Helper()
	executor, calls := newTestContainerUpgrade(t, runningImage)
	executor.instanceID = "restarted"
	dir := t.TempDir()
	running := writeTestFile(t, filepath.Join(dir, "running"), "candidate agent")
	machine := writeTestFile(t, filepath.Join(dir, "machine"), machineBinary)
	executor.runningExecutable = func() (string, error) { return running, nil }
	executor.nspawnBinaryPath = func(string) string { return machine }
	phases := &[]machinav1alpha3.OperationPhase{}
	executor.finishMachineOperation = func(_ context.Context, _ client.Client, _ agentdaemon.MachineOperation, result agentdaemon.MachineOperationResult[int64]) error {
		*phases = append(*phases, result.Phase)
		return nil
	}
	if err := executor.signals.write(agentUpgradeSignal{
		OperationName:            "upgrade",
		ActiveMachine:            "kube1",
		InitiatingDaemonInstance: "initiator",
		CandidateImage:           testAgentImageV2,
		PreviousImage:            testAgentImageV1,
		SwitchCommitted:          true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := executor.images.write(testAgentImageV2, testAgentImageV1); err != nil {
		t.Fatal(err)
	}
	return executor, calls, phases
}

func TestPublishContainerAgentUpgrade(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		runningImage  string
		machineBinary string
		wantPhases    []machinav1alpha3.OperationPhase
		wantSelected  string
		wantLastGood  string
	}{
		"completes when the candidate runs, with its binary in the machine": {
			runningImage:  testAgentImageV2,
			machineBinary: "candidate agent",
			wantPhases:    []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseComplete},
			wantSelected:  testAgentImageV2,
			wantLastGood:  testAgentImageV1,
		},
		"fails and restores the previous image when the unit restarted on it": {
			runningImage:  testAgentImageV1,
			machineBinary: "candidate agent",
			wantPhases:    []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseFailed},
			wantSelected:  testAgentImageV1,
			wantLastGood:  testAgentImageV1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			executor, calls, phases := startedContainerUpgrade(t, tc.runningImage, tc.machineBinary)
			if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), nil, executor); err != nil {
				t.Fatalf("publish: %v", err)
			}
			if !slices.Equal(*phases, tc.wantPhases) {
				t.Errorf("published phases = %v, want %v", *phases, tc.wantPhases)
			}
			if len(*calls) != 0 {
				t.Errorf("calls = %q, want none", *calls)
			}
			assertSelectedImages(t, executor.images, tc.wantSelected, tc.wantLastGood)
			if signal, err := executor.signals.read(); err != nil || signal != nil {
				t.Errorf("signal after publishing = %#v, %v", signal, err)
			}
		})
	}
}

func TestPublishContainerAgentUpgradeFailureRestartsIntoLastGoodBeforeClearingSignal(t *testing.T) {
	t.Parallel()

	// The candidate runs, but the machine's agent binary isn't its own.
	executor, calls, phases := startedContainerUpgrade(t, testAgentImageV2, "another agent")
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), nil, executor); err != nil {
		t.Fatalf("publish candidate recovery: %v", err)
	}
	assertSelectedImages(t, executor.images, testAgentImageV1, testAgentImageV1)
	if signal, err := executor.signals.read(); err != nil || signal == nil || !signal.RecoveryRequired {
		t.Fatalf("signal before the last-good daemon starts = %#v, %v", signal, err)
	}
	if len(*calls) != 1 || !strings.Contains((*calls)[0], "--on-active=1s /usr/bin/systemctl restart "+ServiceUnitName) {
		t.Fatalf("calls = %q, want one scheduled restart", *calls)
	}
	if len(*phases) != 0 {
		t.Fatalf("published %v before the last-good daemon started", *phases)
	}

	executor.instanceID = "last-good"
	executor.runningImage = testAgentImageV1
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), nil, executor); err != nil {
		t.Fatalf("publish last-good recovery: %v", err)
	}
	if !slices.Equal(*phases, []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseFailed}) {
		t.Errorf("published phases = %v, want Failed", *phases)
	}
	if signal, err := executor.signals.read(); err != nil || signal != nil {
		t.Errorf("signal after the last-good daemon = %#v, %v", signal, err)
	}
}

func TestRecoverContainerAgentUpgrade(t *testing.T) {
	t.Parallel()

	resetFailed := "systemctl reset-failed " + ServiceUnitName
	isFailed := "systemctl is-failed --quiet " + ServiceUnitName
	tests := map[string]struct {
		pending      bool
		resetFails   bool
		unitFailed   bool
		wantCalls    []string
		wantSelected string
	}{
		"leaves an ordinary failure alone": {
			wantSelected: testAgentImageV2,
		},
		"restores the last-good image and restarts the agent": {
			pending:      true,
			wantCalls:    []string{resetFailed, "systemctl --no-block restart " + ServiceUnitName},
			wantSelected: testAgentImageV1,
		},
		// As kernel_t, under the development SELinux workaround.
		"waits out the start limit when it can't reset a failed unit": {
			pending:      true,
			resetFails:   true,
			unitFailed:   true,
			wantCalls:    []string{resetFailed, isFailed, "systemd-run "},
			wantSelected: testAgentImageV1,
		},
		"leaves a restarting unit to restart onto the restored image": {
			pending:      true,
			resetFails:   true,
			wantCalls:    []string{resetFailed, isFailed},
			wantSelected: testAgentImageV1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			executor, calls := newTestContainerUpgrade(t, testAgentImageV2)
			if err := executor.images.write(testAgentImageV2, testAgentImageV1); err != nil {
				t.Fatal(err)
			}
			if tc.pending {
				if err := executor.signals.write(agentUpgradeSignal{
					OperationName:   "upgrade",
					ActiveMachine:   "kube1",
					CandidateImage:  testAgentImageV2,
					PreviousImage:   testAgentImageV1,
					SwitchCommitted: true,
				}); err != nil {
					t.Fatal(err)
				}
			}
			systemctl := func(_ context.Context, args ...string) error {
				*calls = append(*calls, "systemctl "+strings.Join(args, " "))
				switch {
				case tc.resetFails && args[0] == "reset-failed":
					return os.ErrPermission
				case args[0] == "is-failed" && !tc.unitFailed:
					return os.ErrNotExist
				}
				return nil
			}

			if err := recoverContainerAgentUpgrade(t.Context(), discardLogger(), "failed repeatedly",
				executor.signals, executor.images, systemctl, executor.runSystemdRun); err != nil {
				t.Fatalf("recover: %v", err)
			}
			if len(*calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", *calls, tc.wantCalls)
			}
			for i, want := range tc.wantCalls {
				if !strings.HasPrefix((*calls)[i], want) {
					t.Errorf("call %d = %q, want it to start with %q", i, (*calls)[i], want)
				}
			}
			if tc.unitFailed && !strings.Contains((*calls)[2], "--on-active=300s") {
				t.Errorf("restart %q isn't scheduled after the start limit", (*calls)[2])
			}
			current, _, err := executor.images.read()
			if err != nil || current != tc.wantSelected {
				t.Errorf("selected image = %q, %v; want %q", current, err, tc.wantSelected)
			}
			signal, err := executor.signals.read()
			if tc.pending && (err != nil || signal == nil || !signal.RecoveryRequired || signal.Failure == "") {
				t.Errorf("signal after recovery = %#v, %v; want the failure recorded", signal, err)
			}
		})
	}
}

func TestContainerAgentConvergence(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state       *State
		machine     bool
		wantBinary  string
		wantReloads int
	}{
		"writes the units and copies the agent into the machine, once": {
			state:       &State{ActiveMachine: "kube1"},
			machine:     true,
			wantBinary:  "running agent",
			wantReloads: 1,
		},
		"skips a machine that isn't there": {
			state:       &State{ActiveMachine: "kube1"},
			wantReloads: 1,
		},
		"skips the machine before the node is started": {
			machine:     true,
			wantBinary:  "older agent",
			wantReloads: 1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			running := writeTestFile(t, filepath.Join(dir, "running"), "running agent")
			machineBinary := filepath.Join(dir, "machines", "kube1", "usr", "local", "bin", "aks-flex-node")
			if tc.machine {
				writeTestFile(t, machineBinary, "older agent")
			}
			reloads := 0
			convergence := &containerAgentConvergence{
				log:               discardLogger(),
				state:             &fakeNodeOperator{state: tc.state},
				unitDir:           filepath.Join(dir, "systemd"),
				reload:            func(context.Context) error { reloads++; return nil },
				runningExecutable: func() (string, error) { return running, nil },
				nspawnBinaryPath:  func(string) string { return machineBinary },
			}
			for run := 1; run <= 2; run++ {
				if err := convergence.converge(t.Context()); err != nil {
					t.Fatalf("converge, run %d: %v", run, err)
				}
			}
			if reloads != tc.wantReloads {
				t.Errorf("reloads = %d, want %d", reloads, tc.wantReloads)
			}
			for unit, want := range map[string][]byte{
				ServiceUnitName:         containerServiceUnitContent,
				recoveryServiceUnitName: containerRecoveryUnitContent,
			} {
				content, err := os.ReadFile(filepath.Join(convergence.unitDir, unit))
				if err != nil || string(content) != string(want) {
					t.Errorf("%s isn't the container's: %v", unit, err)
				}
			}
			content, err := os.ReadFile(machineBinary)
			switch {
			case tc.wantBinary == "" && !os.IsNotExist(err):
				t.Errorf("the machine's agent binary exists: %v", err)
			case tc.wantBinary != "" && string(content) != tc.wantBinary:
				t.Errorf("the machine's agent binary = %q, %v; want %q", content, err, tc.wantBinary)
			}
		})
	}
}
