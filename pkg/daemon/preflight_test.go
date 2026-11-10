package daemon

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestCheckMachinesTarget(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		state          string
		err            error
		failForMissing bool
		want           preflight.Severity
		wantMessage    string
	}{
		"enabled":               {state: "enabled\n", want: preflight.SeverityOK},
		"static":                {state: "static", want: preflight.SeverityOK},
		"disabled":              {state: "disabled", want: preflight.SeverityWarning},
		"runtime only":          {state: "enabled-runtime", want: preflight.SeverityWarning},
		"masked":                {state: "masked", want: preflight.SeverityError},
		"runtime masked":        {state: "masked-runtime", want: preflight.SeverityError},
		"masked strict":         {state: "masked", failForMissing: true, want: preflight.SeverityError},
		"runtime masked strict": {state: "masked-runtime", failForMissing: true, want: preflight.SeverityError},
		"missing online":        {want: preflight.SeverityWarning, wantMessage: "may be installed by online bootstrap with systemd-container"},
		"missing strict":        {failForMissing: true, want: preflight.SeverityError, wantMessage: "install systemd-container on the host before offline or containerized bootstrap"},
		"not found online":      {state: "not-found", want: preflight.SeverityWarning},
		"not found strict":      {state: "not-found", failForMissing: true, want: preflight.SeverityError, wantMessage: "install systemd-container on the host before offline or containerized bootstrap"},
		"unexpected unit state": {state: "linked", want: preflight.SeverityError},
		"inspect failure":       {err: errors.New("systemd unavailable"), want: preflight.SeverityError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			check := machinesTargetChecker{
				log:            slog.Default(),
				failForMissing: tt.failForMissing,
				output: func(_ context.Context, _ *slog.Logger, command string, args ...string) (string, error) {
					if command != "systemctl" || !slices.Equal(args, []string{"show", "--property=UnitFileState", "--value", "machines.target"}) {
						t.Fatalf("unexpected command: %s %v", command, args)
					}
					return tt.state, tt.err
				},
			}
			for range 2 {
				results := check.Check(t.Context())
				if len(results) != 1 || results[0].Severity != tt.want || results[0].Name != "machines-target" {
					t.Fatalf("Check() = %#v, want severity %s", results, tt.want)
				}
				if tt.state == "linked" && !strings.Contains(results[0].Message, tt.state) {
					t.Fatalf("Check() message = %q, want it to include UnitFileState %q", results[0].Message, tt.state)
				}
				if !strings.Contains(results[0].Message, tt.wantMessage) {
					t.Fatalf("Check() message = %q, want it to include %q", results[0].Message, tt.wantMessage)
				}
			}
		})
	}
}

func TestCheckMachinesTargetBootstrapModes(t *testing.T) {
	t.Parallel()

	modes := map[string]struct {
		containerized bool
		offline       bool
		wantMissing   preflight.Severity
	}{
		"native online":         {wantMissing: preflight.SeverityWarning},
		"native offline":        {offline: true, wantMissing: preflight.SeverityError},
		"containerized online":  {containerized: true, wantMissing: preflight.SeverityError},
		"containerized offline": {containerized: true, offline: true, wantMissing: preflight.SeverityError},
	}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, state := range []string{"", "not-found", "masked", "masked-runtime"} {
				t.Run("state="+state, func(t *testing.T) {
					t.Parallel()

					check, ok := CheckMachinesTarget(slog.Default(), mode.containerized || mode.offline).(machinesTargetChecker)
					if !ok {
						t.Fatal("CheckMachinesTarget() returned an unexpected checker type")
					}
					check.output = func(context.Context, *slog.Logger, string, ...string) (string, error) {
						return state, nil
					}
					want := preflight.SeverityError
					if state == "" || state == "not-found" {
						want = mode.wantMissing
					}
					results := check.Check(t.Context())
					if len(results) != 1 || results[0].Severity != want {
						t.Fatalf("Check() = %#v, want severity %s", results, want)
					}
					if (state == "" || state == "not-found") && want == preflight.SeverityError &&
						!strings.Contains(results[0].Message, "install systemd-container on the host") {
						t.Fatalf("Check() message = %q, want host package provisioning guidance", results[0].Message)
					}
				})
			}
		})
	}
}

func TestDaemonUnitsWantMachinesTarget(t *testing.T) {
	t.Parallel()

	hostUnit, err := renderAgentServiceUnit("/usr/local/bin/aks-flex-node", agentServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		"host":      hostUnit,
		"container": containerServiceUnitContent,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, line := range strings.Split(string(content), "\n") {
				if wants, ok := strings.CutPrefix(line, "Wants="); ok && slices.Contains(strings.Fields(wants), "machines.target") {
					return
				}
			}
			t.Fatal("daemon unit must want machines.target so workers start even when the target is disabled")
		})
	}
}
