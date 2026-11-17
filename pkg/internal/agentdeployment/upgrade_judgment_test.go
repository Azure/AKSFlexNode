package agentdeployment

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostUpgradeJudgmentMechanics(t *testing.T) {
	t.Parallel()

	paths := testAgentUpgradePaths(t)
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, paths.BluePath, "candidate")
	writeExecutable(t, paths.GreenPath, "last-good")
	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(paths.GreenPath, paths.LastGoodPath); err != nil {
		t.Fatal(err)
	}
	worker := filepath.Join(t.TempDir(), "worker-agent")
	writeExecutable(t, worker, "candidate")
	executor := &HostUpgrade{
		paths:             paths,
		nspawnBinaryPath:  func(string) string { return worker },
		runningExecutable: func() (string, error) { return paths.BluePath, nil },
	}
	signal := &UpgradeSignal{
		OperationName:   "upgrade",
		ActiveMachine:   "kube1",
		CandidatePath:   paths.BluePath,
		SwitchCommitted: true,
	}
	if err := executor.ValidateStarted(signal); err != nil {
		t.Fatalf("validate candidate: %v", err)
	}
	active, err := executor.CandidateActive(signal)
	if err != nil || !active {
		t.Fatalf("candidate active = %v, %v", active, err)
	}
	lastGood, err := executor.RunningLastGood(signal)
	if err != nil || lastGood {
		t.Fatalf("candidate is last-good = %v, %v", lastGood, err)
	}

	// Do not write into a real machine while testing host-link rollback.
	signal.ActiveMachine = ""
	if err := executor.RollbackSwitch(signal); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	assertResolvedPath(t, paths.CurrentPath, paths.GreenPath)
	executor.runningExecutable = func() (string, error) { return paths.GreenPath, nil }
	lastGood, err = executor.RunningLastGood(signal)
	if err != nil || !lastGood {
		t.Fatalf("last-good running = %v, %v", lastGood, err)
	}
}

func TestContainerUpgradeRecoveryBeforeStageKeepsImageSelection(t *testing.T) {
	t.Parallel()

	executor, calls := newTestContainerUpgrade(t, testAgentImageV2)
	if err := executor.images.write(testAgentImageV2, testAgentImageV1); err != nil {
		t.Fatal(err)
	}
	if err := executor.signals.RecordPending("upgrade", "kube1", "initiator"); err != nil {
		t.Fatal(err)
	}

	if err := recoverContainerAgentUpgrade(t.Context(), discardLogger(), "failed repeatedly",
		executor.signals, executor.images, executor.systemctl); err != nil {
		t.Fatalf("recover: %v", err)
	}
	assertSelectedImages(t, executor.images, testAgentImageV2, testAgentImageV1)
	signal, err := executor.signals.Read()
	if err != nil || signal == nil || !signal.RecoveryRequired || signal.CandidateImage != "" || signal.PreviousImage != "" {
		t.Fatalf("signal after recovery = %#v, %v", signal, err)
	}
	if len(*calls) != 2 || (*calls)[0] != "systemctl reset-failed "+ServiceUnitName ||
		(*calls)[1] != "systemctl --no-block restart "+ServiceUnitName {
		t.Fatalf("recovery calls = %v", *calls)
	}

	if err := executor.RollbackSwitch(signal); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	lastGood, err := executor.RunningLastGood(signal)
	if err != nil || !lastGood {
		t.Fatalf("running image is last-good = %v, %v", lastGood, err)
	}
	assertSelectedImages(t, executor.images, testAgentImageV2, testAgentImageV1)
	if len(*calls) != 2 {
		t.Fatalf("judgment made extra system calls: %v", *calls)
	}
}

func TestContainerUpgradeJudgmentMechanics(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		running      string
		worker       string
		wantValidate bool
		wantLastGood bool
	}{
		"candidate and worker match": {
			running:      testAgentImageV2,
			worker:       "candidate agent",
			wantValidate: true,
		},
		"previous image running": {
			running:      testAgentImageV1,
			worker:       "candidate agent",
			wantLastGood: true,
		},
		"candidate has stale worker": {
			running: testAgentImageV2,
			worker:  "another agent",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			executor, calls := newTestContainerUpgrade(t, tc.running)
			running := writeTestFile(t, filepath.Join(t.TempDir(), "running"), "candidate agent")
			worker := writeTestFile(t, filepath.Join(t.TempDir(), "worker"), tc.worker)
			executor.runningExecutable = func() (string, error) { return running, nil }
			executor.nspawnBinaryPath = func(string) string { return worker }
			if err := executor.images.write(testAgentImageV2, testAgentImageV1); err != nil {
				t.Fatal(err)
			}
			signal := &UpgradeSignal{
				OperationName:   "upgrade",
				ActiveMachine:   "kube1",
				CandidateImage:  testAgentImageV2,
				PreviousImage:   testAgentImageV1,
				SwitchCommitted: true,
			}
			if err := executor.ValidateStarted(signal); (err == nil) != tc.wantValidate {
				t.Fatalf("validate error = %v, want success %v", err, tc.wantValidate)
			}
			active, err := executor.CandidateActive(signal)
			if err != nil || !active {
				t.Fatalf("candidate active = %v, %v", active, err)
			}
			lastGood, err := executor.RunningLastGood(signal)
			if err != nil || lastGood != tc.wantLastGood {
				t.Fatalf("last-good running = %v, %v; want %v", lastGood, err, tc.wantLastGood)
			}
			if tc.wantValidate {
				assertSelectedImages(t, executor.images, testAgentImageV2, testAgentImageV1)
				return
			}
			if err := executor.RollbackSwitch(signal); err != nil {
				t.Fatalf("rollback: %v", err)
			}
			assertSelectedImages(t, executor.images, testAgentImageV1, testAgentImageV1)
			if tc.wantLastGood {
				if len(*calls) != 0 {
					t.Fatalf("unexpected calls: %v", *calls)
				}
				return
			}
			if err := executor.Restart(context.Background()); err != nil {
				t.Fatalf("restart: %v", err)
			}
			if len(*calls) != 2 || (*calls)[0] != "systemctl reset-failed "+ServiceUnitName ||
				!strings.Contains((*calls)[1], "--on-active=1000ms ") ||
				!strings.HasSuffix((*calls)[1], "/usr/bin/systemctl restart "+ServiceUnitName) {
				t.Fatalf("calls = %q, want reset-failed then scheduled restart", *calls)
			}
			executor.runningImage = testAgentImageV1
			lastGood, err = executor.RunningLastGood(signal)
			if err != nil || !lastGood {
				t.Fatalf("last-good running = %v, %v", lastGood, err)
			}
		})
	}
}
