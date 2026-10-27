package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/AKSFlexNode/pkg/config"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const (
	testAgentImageV1 = "registry.example/aks-flex-node-agent:v1"
	// testAgentImageV2 is the image testAgentUpgradeV2 names.
	testAgentImageV2       = "registry.example/aks-flex-node-agent:v2@sha256:" + testAgentImageV2Digest
	testAgentImageV2Digest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
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
		images:        agentImageEnvFile{path: filepath.Join(dir, "agent-image.env")},
		runningImage:  runningImage,
		pullArgs:      []string{"--plain-http"},
		runArgs:       []string{"--snapshotter", "native"},
		ctr:           record("ctr"),
		systemctl:     record("systemctl"),
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

func assertSelectedImages(t *testing.T, images agentImageEnvFile, wantCurrent, wantLastGood string) {
	t.Helper()
	current, lastGood, err := images.read()
	if err != nil {
		t.Fatalf("read the image selection: %v", err)
	}
	if current != wantCurrent || lastGood != wantLastGood {
		t.Errorf("selected images = %q, last good %q; want %q, last good %q", current, lastGood, wantCurrent, wantLastGood)
	}
}

// testAgentUpgradeV2 is an AgentUpgrade to testAgentImageV2.
var testAgentUpgradeV2 = agentUpgradeRequest{
	downloadURL: "oci://registry.example/aks-flex-node-agent:v2",
	sha256:      testAgentImageV2Digest,
}

func TestAgentUpgradeExecutorsTakeTheirOwnKindOfRequest(t *testing.T) {
	t.Parallel()

	archive := agentUpgradeRequest{downloadURL: "https://example.com/agent.tar.gz", sha256: strings.Repeat("a", 64)}
	tests := map[string]struct {
		executor agentUpgradeExecutor
		request  agentUpgradeRequest
		wantErr  string
	}{
		"a host takes an archive": {executor: &hostAgentUpgradeExecutor{}, request: archive},
		// As main, which takes only archives, refuses any URL that isn't HTTP or HTTPS.
		"a host refuses an image":      {executor: &hostAgentUpgradeExecutor{}, request: testAgentUpgradeV2, wantErr: "must use HTTP or HTTPS"},
		"a container takes an image":   {executor: &containerAgentUpgradeExecutor{}, request: testAgentUpgradeV2},
		"a container refuses archives": {executor: &containerAgentUpgradeExecutor{}, request: archive, wantErr: "runs in its container"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := tc.executor.Validate(tc.request)
			if (err != nil) != (tc.wantErr != "") || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestAgentUpgradeImage(t *testing.T) {
	t.Parallel()

	digest := strings.Repeat("a", 64)
	tests := map[string]struct {
		downloadURL string
		sha256      string
		want        string
		wantErr     string
	}{
		"a repository": {
			downloadURL: "oci://mcr.microsoft.com/aks/flex-node",
			sha256:      digest,
			want:        "mcr.microsoft.com/aks/flex-node@sha256:" + digest,
		},
		"a repository and tag, with a port": {
			downloadURL: "oci://registry.example:5000/aks-flex-node-agent:v2",
			sha256:      digest,
			want:        "registry.example:5000/aks-flex-node-agent:v2@sha256:" + digest,
		},
		// As a host takes an archive's, and as any URL's scheme.
		"a prefixed digest, in capitals, and the scheme in capitals": {
			downloadURL: "OCI://mcr.microsoft.com/aks/flex-node",
			sha256:      "sha256:" + strings.ToUpper(digest),
			want:        "mcr.microsoft.com/aks/flex-node@sha256:" + digest,
		},
		"an archive": {
			downloadURL: "https://example.com/agent.tar.gz",
			sha256:      digest,
			wantErr:     "runs in its container",
		},
		// As an archive needs its SHA-256.
		"no digest": {
			downloadURL: "oci://mcr.microsoft.com/aks/flex-node:v2",
			wantErr:     `missing required parameter "sha256"`,
		},
		"a malformed digest": {
			downloadURL: "oci://mcr.microsoft.com/aks/flex-node",
			sha256:      "bad",
			wantErr:     "64 hexadecimal",
		},
		// The digest comes from one place, as an archive's does.
		"a digest in the URL": {
			downloadURL: "oci://mcr.microsoft.com/aks/flex-node@sha256:" + digest,
			sha256:      digest,
			wantErr:     "not in the URL",
		},
		// ctr pulls only fully qualified references.
		"not fully qualified": {
			downloadURL: "oci://flex-node:v2",
			sha256:      digest,
			wantErr:     "invalid image reference",
		},
		"one that would read as a flag": {
			downloadURL: "oci://--snapshotter=native",
			sha256:      digest,
			wantErr:     "invalid image reference",
		},
		"credentials": {
			downloadURL: "oci://user:secret@mcr.microsoft.com/aks/flex-node",
			sha256:      digest,
			wantErr:     "invalid image reference",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := agentUpgradeImage(agentUpgradeRequest{downloadURL: tc.downloadURL, sha256: tc.sha256})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("agentUpgradeImage() = %q, %v; want error %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("agentUpgradeImage() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestContainerAgentUpgradeStage(t *testing.T) {
	t.Parallel()

	pull := "ctr --namespace aks-flex image pull --label aks-flex-node.agent-upgrade=pulled --plain-http " + testAgentImageV2
	removeTask := "ctr --namespace aks-flex task delete --force aks-flex-node-upgrade-check"
	removeContainer := "ctr --namespace aks-flex container delete aks-flex-node-upgrade-check"
	// The candidate's agent, in a scope of the agent's slice that stops with the agent's unit.
	check := "ctr --namespace aks-flex run --rm --runc-systemd-cgroup --cgroup system-aksflex.slice:aks-flex:upgrade-check " +
		"--annotation org.systemd.property.BindsTo=['aks-flex-node-agent.service'] " +
		"--annotation org.systemd.property.TimeoutStopUSec=uint64 30000000 " +
		"--annotation org.systemd.property.CollectMode='inactive-or-failed' --snapshotter native " +
		testAgentImageV2 + " aks-flex-node-upgrade-check aks-flex-node version"
	tests := map[string]struct {
		runningImage   string
		selected       string
		failOn         string
		cancelOnFail   bool
		wantCalls      []string
		wantErr        bool
		wantSignal     bool
		wantPrevious   string
		wantCommitted  bool
		wantSelected   string
		wantLastGood   string
		wantNoSelected bool
	}{
		"switches from the image the host's provisioning names": {
			runningImage:  testAgentImageV1,
			wantCalls:     []string{pull, removeTask, removeContainer, check},
			wantSignal:    true,
			wantPrevious:  testAgentImageV1,
			wantCommitted: true,
			wantSelected:  testAgentImageV2,
			wantLastGood:  testAgentImageV1,
		},
		"switches from the image an earlier upgrade selected": {
			runningImage:  testAgentImageV1,
			selected:      testAgentImageV1,
			wantCalls:     []string{pull, removeTask, removeContainer, check},
			wantSignal:    true,
			wantPrevious:  testAgentImageV1,
			wantCommitted: true,
			wantSelected:  testAgentImageV2,
			wantLastGood:  testAgentImageV1,
		},
		// A rollback would restore an image that never ran here.
		"refuses when another image was selected since the agent started": {
			runningImage:   testAgentImageV1,
			selected:       "registry.example/aks-flex-node-agent@sha256:" + strings.Repeat("c", 64),
			wantErr:        true,
			wantNoSelected: true,
		},
		"doesn't select an image it couldn't pull": {
			runningImage:   testAgentImageV1,
			failOn:         "pull",
			wantCalls:      []string{pull},
			wantErr:        true,
			wantSignal:     true,
			wantPrevious:   testAgentImageV1,
			wantNoSelected: true,
		},
		"doesn't select an image whose agent doesn't run, and removes its container": {
			runningImage:   testAgentImageV1,
			failOn:         "run",
			wantCalls:      []string{pull, removeTask, removeContainer, check, removeTask, removeContainer},
			wantErr:        true,
			wantSignal:     true,
			wantPrevious:   testAgentImageV1,
			wantNoSelected: true,
		},
		// ctr, killed with its context, leaves the container; its removal outlives the context.
		"removes the check's container when the upgrade is cancelled": {
			runningImage:   testAgentImageV1,
			failOn:         "run",
			cancelOnFail:   true,
			wantCalls:      []string{pull, removeTask, removeContainer, check, removeTask, removeContainer},
			wantErr:        true,
			wantSignal:     true,
			wantPrevious:   testAgentImageV1,
			wantNoSelected: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var pullDeadline, checkDeadline time.Duration
			liveCleanup := true
			executor, calls := newTestContainerUpgrade(t, tc.runningImage)
			executor.ctr = func(callCtx context.Context, args ...string) error {
				*calls = append(*calls, "ctr "+strings.Join(args, " "))
				deadline, ok := callCtx.Deadline()
				switch {
				case slices.Contains(args, "pull") && ok:
					pullDeadline = time.Until(deadline)
				case slices.Contains(args, "run") && ok:
					checkDeadline = time.Until(deadline)
				case slices.Contains(args, "delete"):
					liveCleanup = liveCleanup && callCtx.Err() == nil
				}
				if slices.Contains(args, tc.failOn) {
					if tc.cancelOnFail {
						cancel()
						return context.Canceled
					}
					return os.ErrPermission
				}
				return nil
			}
			if tc.selected != "" {
				if err := executor.images.write(tc.selected, tc.selected); err != nil {
					t.Fatal(err)
				}
			}
			if err := executor.RecordPending(ctx, "upgrade"); err != nil {
				t.Fatalf("RecordPending: %v", err)
			}

			err := executor.Stage(ctx, testAgentUpgradeV2)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Stage() error = %v, want error %v", err, tc.wantErr)
			}
			if !slices.Equal(*calls, tc.wantCalls) {
				t.Errorf("calls = %q, want %q", *calls, tc.wantCalls)
			}
			// As a host limits its download and its candidate's version check.
			if len(*calls) > 0 && (pullDeadline <= 0 || pullDeadline > agentImagePullTimeout) {
				t.Errorf("the pull's deadline is %v away, want within %v", pullDeadline, agentImagePullTimeout)
			}
			if len(*calls) > 3 && (checkDeadline <= 0 || checkDeadline > agentImageCheckTimeout) {
				t.Errorf("the check's deadline is %v away, want within %v", checkDeadline, agentImageCheckTimeout)
			}
			if !liveCleanup {
				t.Error("the check's container was removed with a cancelled context")
			}
			signal, err := executor.signals.read()
			if err != nil || signal == nil {
				t.Fatalf("signal = %#v, %v", signal, err)
			}
			if tc.wantSignal && (signal.CandidateImage != testAgentImageV2 || signal.PreviousImage != tc.wantPrevious ||
				signal.SwitchCommitted != tc.wantCommitted) {
				t.Errorf("signal = %#v", signal)
			}
			if !tc.wantSignal && signal.CandidateImage != "" {
				t.Errorf("signal = %#v, want no candidate recorded", signal)
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
	if err := executor.Stage(t.Context(), testAgentUpgradeV2); err != nil {
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
	// The restart into the last-good image doesn't count toward the unit's start limit.
	if len(*calls) != 2 || (*calls)[0] != "systemctl reset-failed "+ServiceUnitName ||
		!strings.Contains((*calls)[1], "--on-active=1000ms ") ||
		!strings.HasSuffix((*calls)[1], "/usr/bin/systemctl restart "+ServiceUnitName) {
		t.Fatalf("calls = %q, want the unit's start limit reset, then one scheduled restart", *calls)
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

// TestContainerAgentUpgradeInterruptedBeforeStage: the daemon ends between RecordPending and
// Stage, recovery runs for the failed unit, and the next daemon judges the upgrade. Nothing
// switched, so the upgrade fails, the selection stays, and the signal goes.
func TestContainerAgentUpgradeInterruptedBeforeStage(t *testing.T) {
	t.Parallel()

	executor, calls := newTestContainerUpgrade(t, testAgentImageV2)
	executor.instanceID = "initiator"
	if err := executor.images.write(testAgentImageV2, testAgentImageV1); err != nil {
		t.Fatal(err)
	}
	if err := executor.RecordPending(t.Context(), "upgrade"); err != nil {
		t.Fatalf("RecordPending: %v", err)
	}
	if err := recoverContainerAgentUpgrade(t.Context(), discardLogger(), "failed repeatedly",
		executor.signals, executor.images, executor.systemctl); err != nil {
		t.Fatalf("recover: %v", err)
	}
	assertSelectedImages(t, executor.images, testAgentImageV2, testAgentImageV1)

	*calls = nil
	executor.instanceID = "restarted"
	var phases []machinav1alpha3.OperationPhase
	executor.finishMachineOperation = func(_ context.Context, _ client.Client, _ agentdaemon.MachineOperation, result agentdaemon.MachineOperationResult[int64]) error {
		phases = append(phases, result.Phase)
		return nil
	}
	if err := publishAndClearAgentUpgradeSignal(t.Context(), discardLogger(), nil, executor); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !slices.Equal(phases, []machinav1alpha3.OperationPhase{machinav1alpha3.OperationPhaseFailed}) {
		t.Errorf("published phases = %v, want Failed", phases)
	}
	if len(*calls) != 0 {
		t.Errorf("calls = %q, want none: the agent already runs its image", *calls)
	}
	assertSelectedImages(t, executor.images, testAgentImageV2, testAgentImageV1)
	if signal, err := executor.signals.read(); err != nil || signal != nil {
		t.Errorf("signal after publishing = %#v, %v", signal, err)
	}
}

func TestRecoverContainerAgentUpgrade(t *testing.T) {
	t.Parallel()

	resetFailed := "systemctl reset-failed " + ServiceUnitName
	tests := map[string]struct {
		pending      bool
		interrupted  bool
		resetFails   bool
		wantCalls    []string
		wantSelected string
		wantErr      bool
	}{
		"leaves an ordinary failure alone": {
			wantSelected: testAgentImageV2,
		},
		// The daemon ended between RecordPending and Stage, so the upgrade never switched.
		"restarts the agent on its image when the upgrade never chose a candidate": {
			pending:      true,
			interrupted:  true,
			wantCalls:    []string{resetFailed, "systemctl --no-block restart " + ServiceUnitName},
			wantSelected: testAgentImageV2,
		},
		"restores the last-good image and restarts the agent": {
			pending:      true,
			wantCalls:    []string{resetFailed, "systemctl --no-block restart " + ServiceUnitName},
			wantSelected: testAgentImageV1,
		},
		// The image is restored, for the next start of the unit, however it comes about.
		"reports a unit it can't reset": {
			pending:      true,
			resetFails:   true,
			wantCalls:    []string{resetFailed},
			wantSelected: testAgentImageV1,
			wantErr:      true,
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
				signal := agentUpgradeSignal{
					OperationName:   "upgrade",
					ActiveMachine:   "kube1",
					CandidateImage:  testAgentImageV2,
					PreviousImage:   testAgentImageV1,
					SwitchCommitted: true,
				}
				if tc.interrupted {
					signal = agentUpgradeSignal{OperationName: "upgrade", ActiveMachine: "kube1"}
				}
				if err := executor.signals.write(signal); err != nil {
					t.Fatal(err)
				}
			}
			systemctl := func(_ context.Context, args ...string) error {
				*calls = append(*calls, "systemctl "+strings.Join(args, " "))
				if tc.resetFails && args[0] == "reset-failed" {
					return os.ErrPermission
				}
				return nil
			}
			err := recoverContainerAgentUpgrade(t.Context(), discardLogger(), "failed repeatedly",
				executor.signals, executor.images, systemctl)
			if (err != nil) != tc.wantErr {
				t.Fatalf("recover: %v, want error %v", err, tc.wantErr)
			}
			if len(*calls) != len(tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", *calls, tc.wantCalls)
			}
			for i, want := range tc.wantCalls {
				if !strings.HasPrefix((*calls)[i], want) {
					t.Errorf("call %d = %q, want it to start with %q", i, (*calls)[i], want)
				}
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
			reloads, retired := 0, 0
			convergence := &containerAgentConvergence{
				log:               discardLogger(),
				state:             &fakeNodeOperator{state: tc.state},
				unitDir:           filepath.Join(dir, "systemd"),
				signalPath:        filepath.Join(dir, "agent-upgrade-signal.json"),
				reload:            func(context.Context) error { reloads++; return nil },
				runningExecutable: func() (string, error) { return running, nil },
				nspawnBinaryPath:  func(string) string { return machineBinary },
				images:            agentImageEnvFile{path: filepath.Join(dir, "agent-image.env")},
				install:           agentImageEnvFile{path: writeTestFile(t, filepath.Join(dir, "settings"), "AKS_FLEX_NODE_IMAGE="+testAgentImageV1+"\n")},
				runningImage:      testAgentImageV1,
				listImages:        func(context.Context) (string, error) { return testAgentImageV1 + "\n", nil },
				ctr: func(_ context.Context, args ...string) error {
					t.Errorf("unexpected ctr %q", args)
					return nil
				},
				retireHelper: func(context.Context) error { retired++; return nil },
			}
			for run := 1; run <= 2; run++ {
				if _, err := convergence.converge(t.Context()); err != nil {
					t.Fatalf("converge, run %d: %v", run, err)
				}
			}
			if reloads != tc.wantReloads {
				t.Errorf("reloads = %d, want %d", reloads, tc.wantReloads)
			}
			if retired != 2 {
				t.Errorf("retired the lifecycle helper %d times, want 2, once per start", retired)
			}
			for unit, want := range map[string][]byte{
				ServiceUnitName:            containerServiceUnitContent,
				recoveryServiceUnitName:    containerRecoveryUnitContent,
				config.NSpawnPreStartUnit:  containerNSpawnPreStartUnitContent,
				config.NSpawnPostStartUnit: containerNSpawnPostStartUnitContent,
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

func TestContainerAgentRemovesOldImages(t *testing.T) {
	t.Parallel()

	const (
		install = "registry.example/aks-flex-node-agent:v0"
		old     = "registry.example/aks-flex-node-agent:v-old"
		broken  = "registry.example/aks-flex-node-agent:broken"
	)
	listed := strings.Join([]string{install, testAgentImageV1, testAgentImageV2, old, broken}, "\n")
	tests := map[string]struct {
		selection    string
		settings     string
		runningImage string
		listErr      error
		failRemoval  string
		wantRemoved  []string
	}{
		"keeps the selected, last good, install's, and running images": {
			selection:    "AKS_FLEX_NODE_IMAGE=" + testAgentImageV2 + "\nAKS_FLEX_NODE_LAST_GOOD_IMAGE=" + testAgentImageV1 + "\n",
			settings:     "AKS_FLEX_NODE_IMAGE=" + install + "\nAKS_FLEX_NODE_PULL_ARGS=--plain-http\n",
			runningImage: testAgentImageV2,
			wantRemoved:  []string{old, broken},
		},
		"without a selection, keeps the install's image, however it's quoted": {
			settings:     `AKS_FLEX_NODE_IMAGE="` + install + `"` + "\n",
			runningImage: install,
			wantRemoved:  []string{testAgentImageV1, testAgentImageV2, old, broken},
		},
		"removes none when the install's image is unknown": {
			runningImage: testAgentImageV2,
		},
		"removes none when it can't list the images": {
			settings: "AKS_FLEX_NODE_IMAGE=" + install + "\n",
			listErr:  os.ErrPermission,
		},
		"a failed removal doesn't stop the others": {
			settings:     "AKS_FLEX_NODE_IMAGE=" + install + "\n",
			runningImage: install,
			failRemoval:  testAgentImageV1,
			wantRemoved:  []string{testAgentImageV1, testAgentImageV2, old, broken},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			var removed []string
			// The daemon removes images as it starts, so a call that hung would hold it there.
			bounded := func(ctx context.Context, call string) {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > agentImageCommandTimeout {
					t.Errorf("%s has no deadline within %v", call, agentImageCommandTimeout)
				}
			}
			convergence := &containerAgentConvergence{
				log:          discardLogger(),
				images:       agentImageEnvFile{path: filepath.Join(dir, "agent-image.env")},
				install:      agentImageEnvFile{path: filepath.Join(dir, "settings")},
				runningImage: tc.runningImage,
				listImages: func(ctx context.Context) (string, error) {
					bounded(ctx, "listing the images")
					return listed, tc.listErr
				},
				ctr: func(ctx context.Context, args ...string) error {
					bounded(ctx, "ctr "+strings.Join(args, " "))
					if !slices.Equal(args[:4], []string{"--namespace", "aks-flex", "images", "rm"}) {
						t.Errorf("unexpected ctr %q", args)
					}
					removed = append(removed, args[4])
					if args[4] == tc.failRemoval {
						return os.ErrPermission
					}
					return nil
				},
			}
			if tc.selection != "" {
				writeTestFile(t, convergence.images.path, tc.selection)
			}
			if tc.settings != "" {
				writeTestFile(t, convergence.install.path, tc.settings)
			}

			convergence.removeImages(t.Context())
			if !slices.Equal(removed, tc.wantRemoved) {
				t.Errorf("removed %q, want %q", removed, tc.wantRemoved)
			}
		})
	}
}

// The recovery unit comes first, as on a host, so a write that stops partway leaves the agent's
// unit as it was, with its recovery unit in place.
func TestWriteContainerUnitsWritesRecoveryFirst(t *testing.T) {
	t.Parallel()

	unitDir := t.TempDir()
	// The agent's unit can't be written over a non-empty directory.
	if err := os.MkdirAll(filepath.Join(unitDir, ServiceUnitName, "busy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := writeContainerUnits(unitDir, false); err == nil {
		t.Fatal("writeContainerUnits() wrote the agent's unit over a directory")
	}
	recovery, err := os.ReadFile(filepath.Join(unitDir, recoveryServiceUnitName))
	if err != nil || !bytes.Equal(recovery, containerRecoveryUnitContent) {
		t.Errorf("the recovery unit wasn't written before the agent's unit failed: %v", err)
	}
}

// containerUnitNames are the units writeContainerUnits writes.
var containerUnitNames = []string{config.NSpawnPreStartUnit, config.NSpawnPostStartUnit, recoveryServiceUnitName, ServiceUnitName}

// While an AgentUpgrade's signal exists, the daemon may be its candidate, not yet judged, and the
// units on disk are the last-good version's, which recovery runs through, as
// ensureDaemonServiceAssets keeps a host's.
func TestContainerAgentConvergenceHoldsTheUnitsWhileAnUpgradeIsPending(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		pending      bool
		missing      string
		wantHeld     bool
		wantRewrites []string
	}{
		"keeps the last-good version's units": {
			pending:  true,
			wantHeld: true,
		},
		"writes a missing one": {
			pending:      true,
			missing:      recoveryServiceUnitName,
			wantHeld:     true,
			wantRewrites: []string{recoveryServiceUnitName},
		},
		"converges them without a pending upgrade": {
			wantRewrites: containerUnitNames,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			unitDir := filepath.Join(dir, "systemd")
			lastGood := "the last-good version's\n"
			for _, unit := range containerUnitNames {
				if unit != tc.missing {
					writeTestFile(t, filepath.Join(unitDir, unit), lastGood)
				}
			}
			signalPath := filepath.Join(dir, "agent-upgrade-signal.json")
			if tc.pending {
				writeTestFile(t, signalPath, "{}")
			}
			reloads, retired := 0, 0
			convergence := &containerAgentConvergence{
				log:               discardLogger(),
				state:             &fakeNodeOperator{},
				unitDir:           unitDir,
				signalPath:        signalPath,
				reload:            func(context.Context) error { reloads++; return nil },
				runningExecutable: func() (string, error) { return writeTestFile(t, filepath.Join(dir, "running"), "agent"), nil },
				nspawnBinaryPath:  func(string) string { return filepath.Join(dir, "machines", "kube1", "aks-flex-node") },
				images:            agentImageEnvFile{path: filepath.Join(dir, "agent-image.env")},
				install:           agentImageEnvFile{path: filepath.Join(dir, "settings")},
				listImages:        func(context.Context) (string, error) { return "", nil },
				ctr:               func(context.Context, ...string) error { return nil },
				// While the upgrade is pending, the last-good version's hooks may run the helper.
				retireHelper: func(context.Context) error { retired++; return nil },
			}

			held, err := convergence.converge(t.Context())
			if err != nil {
				t.Fatalf("converge: %v", err)
			}
			if held != tc.wantHeld {
				t.Errorf("held = %v, want %v", held, tc.wantHeld)
			}
			if wantRetired := map[bool]int{true: 0, false: 1}[tc.wantHeld]; retired != wantRetired {
				t.Errorf("retired the lifecycle helper %d times, want %d", retired, wantRetired)
			}
			for _, unit := range containerUnitNames {
				content, err := os.ReadFile(filepath.Join(unitDir, unit))
				if err != nil {
					t.Fatalf("read %s: %v", unit, err)
				}
				if rewritten := string(content) != lastGood; rewritten != slices.Contains(tc.wantRewrites, unit) {
					t.Errorf("%s rewritten = %v, want %v", unit, rewritten, !rewritten)
				}
			}

			// Once the upgrade is judged, the daemon writes the units it held back.
			reloads = 0
			if err := convergence.writeUnits(t.Context()); err != nil {
				t.Fatalf("writeUnits: %v", err)
			}
			for unit, want := range map[string][]byte{
				recoveryServiceUnitName:    containerRecoveryUnitContent,
				ServiceUnitName:            containerServiceUnitContent,
				config.NSpawnPreStartUnit:  containerNSpawnPreStartUnitContent,
				config.NSpawnPostStartUnit: containerNSpawnPostStartUnitContent,
			} {
				if content, err := os.ReadFile(filepath.Join(unitDir, unit)); err != nil || !bytes.Equal(content, want) {
					t.Errorf("%s after writeUnits isn't the image's: %v", unit, err)
				}
			}
			if wantRetired := map[bool]int{true: 1, false: 2}[tc.wantHeld]; retired != wantRetired {
				t.Errorf("retired the lifecycle helper %d times after writeUnits, want %d", retired, wantRetired)
			}
			if wantReloads := map[bool]int{true: 1, false: 0}[tc.wantHeld]; reloads != wantReloads {
				t.Errorf("reloads after writeUnits = %d, want %d", reloads, wantReloads)
			}
		})
	}
}
