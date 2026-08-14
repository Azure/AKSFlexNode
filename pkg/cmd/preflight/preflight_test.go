package preflight

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestNewCommand(t *testing.T) {
	t.Parallel()

	cmd := NewCommand()
	if cmd.Use != "preflight" {
		t.Fatalf("Use = %q, want preflight", cmd.Use)
	}

	for _, flag := range []string{"config", "ignore-preflight-errors", "fail-on-warnings", "output"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("expected flag %q", flag)
		}
	}
}

func TestNormalizeOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to text", input: "", want: "text"},
		{name: "text", input: "text", want: "text"},
		{name: "json", input: "json", want: "json"},
		{name: "case insensitive", input: "JSON", want: "json"},
		{name: "trimmed", input: " text ", want: "text"},
		{name: "unsupported", input: "yaml", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeOutput(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeOutput() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeOutput() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeOutput() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWriteText(t *testing.T) {
	t.Parallel()

	report := preflight.Report{
		Checks: []preflight.Result{
			preflight.OK("ok-check", "ok target", "all good"),
			preflight.Warning("warn-check", "warn target", "be careful"),
			preflight.Error("error-check", "error target", "bad thing"),
		},
	}

	var out bytes.Buffer
	if err := writeText(&out, report); err != nil {
		t.Fatalf("writeText() error = %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"[preflight] Running AKS Flex Node preflight checks",
		"[OK ok-check]: all good (target: ok target)",
		"[WARNING warn-check]: be careful (target: warn target)",
		"[ERROR error-check]: bad thing (target: error target)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("writeText() output missing %q\n%s", want, got)
		}
	}
}

func TestResolveMachineGoal(t *testing.T) {
	t.Parallel()

	localGoal := aksmachine.GoalState{
		KubernetesVersion: "1.34.0",
		MaxPods:           30,
		NodeLabels:        map[string]string{"source": "local"},
		NodeTaints:        []string{"dedicated=local:NoSchedule"},
		KubeletConfig: aksmachine.KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
	newRemoteGoal := func() aksmachine.GoalState {
		goal := localGoal
		goal.SettingsVersion = "etag-42"
		goal.NodeLabels = maps.Clone(localGoal.NodeLabels)
		goal.NodeTaints = slices.Clone(localGoal.NodeTaints)
		return goal
	}

	matchingGoal := newRemoteGoal()
	versionMismatch := newRemoteGoal()
	versionMismatch.KubernetesVersion = "1.35.0"
	maxPodsMismatch := newRemoteGoal()
	maxPodsMismatch.MaxPods = 110
	labelsMismatch := newRemoteGoal()
	labelsMismatch.NodeLabels["source"] = "remote"
	taintsMismatch := newRemoteGoal()
	taintsMismatch.NodeTaints = []string{"dedicated=remote:NoExecute"}
	imageGCHighMismatch := newRemoteGoal()
	imageGCHighMismatch.KubeletConfig.ImageGCHighThreshold = 90
	imageGCLowMismatch := newRemoteGoal()
	imageGCLowMismatch.KubeletConfig.ImageGCLowThreshold = 75

	tests := map[string]struct {
		machine    *aksmachine.Machine
		getErr     error
		require    bool
		wantRemote *aksmachine.GoalState
		severity   preflight.Severity
		message    string
		wantErr    string
	}{
		"matching remote goal is used": {
			machine:    &aksmachine.Machine{Goal: matchingGoal},
			wantRemote: &matchingGoal,
			severity:   preflight.SeverityOK,
			message:    "matches local config",
		},
		"Kubernetes version mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: versionMismatch},
			wantRemote: &versionMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"max pods mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: maxPodsMismatch},
			wantRemote: &maxPodsMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"labels mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: labelsMismatch},
			wantRemote: &labelsMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"taints mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: taintsMismatch},
			wantRemote: &taintsMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"image GC high threshold mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: imageGCHighMismatch},
			wantRemote: &imageGCHighMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"image GC low threshold mismatch fails and uses remote": {
			machine:    &aksmachine.Machine{Goal: imageGCLowMismatch},
			wantRemote: &imageGCLowMismatch,
			severity:   preflight.SeverityError,
			message:    "is authoritative and differs from local config",
		},
		"missing machine preserves config-only resolution": {
			getErr:   &aksmachine.NotFoundError{Resource: "machine"},
			severity: preflight.SeverityOK,
			message:  "does not exist",
		},
		"optional read failure warns and preserves config-only resolution": {
			getErr:   errors.New("boom"),
			severity: preflight.SeverityWarning,
			message:  "validating the local bootstrap goal",
			wantErr:  "read AKS Machine: boom",
		},
		"required read failure fails and preserves config-only resolution": {
			getErr:   errors.New("boom"),
			require:  true,
			severity: preflight.SeverityError,
			message:  "machine registration is required",
			wantErr:  "read AKS Machine: boom",
		},
		"optional invalid machine warns and preserves config-only resolution": {
			machine:  &aksmachine.Machine{Goal: aksmachine.GoalState{KubernetesVersion: "1.35.0"}},
			severity: preflight.SeverityWarning,
			message:  "validating the local bootstrap goal",
			wantErr:  "validate AKS Machine: goal settings version is empty",
		},
		"required nil machine fails and preserves config-only resolution": {
			require:  true,
			severity: preflight.SeverityError,
			message:  "machine registration is required",
			wantErr:  "validate AKS Machine: machine is nil",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			client := &preflightMachineClient{machine: tt.machine, getErr: tt.getErr}
			remoteGoal, result, err := resolveMachineGoal(t.Context(), client, localGoal, tt.require)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("resolveMachineGoal() error = %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolveMachineGoal() error = %v, want containing %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(remoteGoal, tt.wantRemote) {
				t.Fatalf("resolveMachineGoal() remote goal = %#v, want %#v", remoteGoal, tt.wantRemote)
			}
			if result.Name != machineGoalCheckName || result.Severity != tt.severity || !strings.Contains(result.Message, tt.message) {
				t.Fatalf("resolveMachineGoal() result = %#v, want severity %q and message containing %q", result, tt.severity, tt.message)
			}
			if client.createCalls != 0 {
				t.Fatalf("Create() calls = %d, want 0", client.createCalls)
			}
		})
	}
}

func TestGoalsMatch(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		remote aksmachine.GoalState
		local  aksmachine.GoalState
		want   bool
	}{
		"settings version is ignored": {
			remote: aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "etag-42"},
			local:  aksmachine.GoalState{KubernetesVersion: "1.34.0"},
			want:   true,
		},
		"nil and empty collections are equivalent": {
			remote: aksmachine.GoalState{KubernetesVersion: "1.34.0", SettingsVersion: "etag-42"},
			local: aksmachine.GoalState{
				KubernetesVersion: "1.34.0",
				NodeLabels:        map[string]string{},
				NodeTaints:        []string{},
			},
			want: true,
		},
		"taint order is ignored": {
			remote: aksmachine.GoalState{
				KubernetesVersion: "1.34.0",
				SettingsVersion:   "etag-42",
				NodeTaints:        []string{"b=true:NoExecute", "a=true:NoSchedule"},
			},
			local: aksmachine.GoalState{
				KubernetesVersion: "1.34.0",
				NodeTaints:        []string{"a=true:NoSchedule", "b=true:NoExecute"},
			},
			want: true,
		},
		"different labels do not match": {
			remote: aksmachine.GoalState{KubernetesVersion: "1.34.0", NodeLabels: map[string]string{"source": "remote"}},
			local:  aksmachine.GoalState{KubernetesVersion: "1.34.0", NodeLabels: map[string]string{"source": "local"}},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := goalsMatch(tt.remote, tt.local); got != tt.want {
				t.Fatalf("goalsMatch() = %t, want %t", got, tt.want)
			}
		})
	}
}

type preflightMachineClient struct {
	machine     *aksmachine.Machine
	getErr      error
	createCalls int
}

func (c *preflightMachineClient) Create(context.Context, aksmachine.GoalState) (*aksmachine.Machine, error) {
	c.createCalls++
	return nil, errors.New("unexpected Create call")
}

func (c *preflightMachineClient) Get(context.Context) (*aksmachine.Machine, error) {
	return c.machine, c.getErr
}

func (*preflightMachineClient) PatchStatus(context.Context, aksmachine.Status) error {
	return errors.New("unexpected PatchStatus call")
}
