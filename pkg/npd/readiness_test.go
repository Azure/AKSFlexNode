package npd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestNPDRestartChecksRunningProcessAfterSuccessfulJob(t *testing.T) {
	t.Parallel()
	showErr := errors.New("systemd status unavailable")
	for _, tc := range []struct {
		name, status string
		showErr      error
		wantErr      bool
	}{
		{name: "running", status: runningNPDStatus},
		{name: "properties reordered", status: "MainPID=321\nSubState=running\nActiveState=active\n"},
		{name: "automatic restart without process", status: "ActiveState=activating\nSubState=auto-restart\nMainPID=0\n", wantErr: true},
		{name: "running without process", status: "ActiveState=active\nSubState=running\nMainPID=0\n", wantErr: true},
		{name: "active but not running", status: "ActiveState=active\nSubState=auto-restart\nMainPID=123\n", wantErr: true},
		{name: "not active", status: "ActiveState=inactive\nSubState=running\nMainPID=123\n", wantErr: true},
		{name: "missing state", status: "SubState=running\nMainPID=123\n", wantErr: true},
		{name: "missing substate", status: "ActiveState=active\nMainPID=123\n", wantErr: true},
		{name: "missing PID", status: "ActiveState=active\nSubState=running\n", wantErr: true},
		{name: "malformed output", status: "active\nrunning\n123\n", wantErr: true},
		{name: "malformed PID", status: "ActiveState=active\nSubState=running\nMainPID=not-a-pid\n", wantErr: true},
		{name: "negative PID", status: "ActiveState=active\nSubState=running\nMainPID=-1\n", wantErr: true},
		{name: "status command failed", showErr: showErr, wantErr: true},
		{name: "status canceled", showErr: context.Canceled, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, hostNative := range []bool{false, true} {
				name := "nspawn"
				if hostNative {
					name = "none"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					root := t.TempDir()
					unit := systemdUnitNPD
					var calls []string
					ns := &goalstates.NodeStart{
						MachineDir: root, NodeName: "worker-1",
						ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
							calls = append(calls, strings.Join(args, " "))
							if len(args) > 1 && args[1] == "show" {
								return tc.status, tc.showErr
							}
							// is-active and restart both succeed, even when the
							// service has not reached a running main process.
							return "active", nil
						},
					}
					if hostNative {
						unit = HostUnitNPD
						ns.RootOverride = none.Root{HostRoot: root, Layout: none.Layout{Base: filepath.Join(root, "node")}, Version: "v1"}
					}
					err := Restart(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(t.Context())
					if (err != nil) != tc.wantErr {
						t.Fatalf("Restart error = %v, wantErr = %t", err, tc.wantErr)
					}
					if tc.showErr != nil && !errors.Is(err, tc.showErr) {
						t.Fatalf("Restart error = %v, want wrapped status failure", err)
					}
					if !slices.Contains(calls, "systemctl restart "+unit) || calls[len(calls)-1] != npdStatusCommand(unit) {
						t.Fatalf("restart was not followed by a status snapshot: %v", calls)
					}
				})
			}
		})
	}
}
