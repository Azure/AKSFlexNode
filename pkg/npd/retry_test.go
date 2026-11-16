package npd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestNPDReadinessAndRestartPolicy(t *testing.T) {
	t.Parallel()
	task, ok := Start(slog.New(slog.DiscardHandler), &config.Config{}, &goalstates.NodeStart{MachineDir: t.TempDir()}).(*startTask)
	if !ok {
		t.Fatal("Start did not return an NPD task")
	}
	if task.pollInterval != time.Second || task.stableFor != 2*time.Second {
		t.Fatalf("unexpected readiness policy: interval=%s stableFor=%s", task.pollInterval, task.stableFor)
	}
	for _, setting := range []string{"Restart=on-failure", "RestartSec=5s"} {
		if !strings.Contains(serviceTemplate, setting) {
			t.Fatalf("NPD unit lacks automatic retry setting %s", setting)
		}
	}
}

func TestNPDWaitsForSystemdRetryAndStableProcess(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nspawn", "none", "container"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			starts, checks := 0, 0
			active := false
			ns := &goalstates.NodeStart{
				MachineDir: root, NodeName: "worker-1",
				Kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "not-ready-at-first-start"}},
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					if args[0] != "systemctl" {
						t.Fatalf("readiness is not a generic systemd check: %v", args)
					}
					switch args[1] {
					case "is-active":
						if !active {
							return "inactive", errors.New("not started")
						}
					case "enable", "restart":
						starts++
						active = true
					case "show":
						checks++
						// The first process briefly looks running, then exits.
						// A later systemd restart supplies the stable process.
						switch checks {
						case 1, 3:
							return "ActiveState=activating\nSubState=auto-restart\nMainPID=0\n", nil
						case 2:
							return "ActiveState=active\nSubState=running\nMainPID=123\n", nil
						default:
							return "ActiveState=active\nSubState=running\nMainPID=456\n", nil
						}
					}
					return "active", nil
				},
			}
			switch mode {
			case "none":
				ns.RootOverride = none.Root{HostRoot: root, Layout: none.Layout{Base: filepath.Join(root, "node")}, Version: "v1"}
			case "container":
				ns.RootOverride = container.Root{SlotName: "kube1", Dir: root, HostRoot: t.TempDir()}
				ns.InContainer = true
			}
			for range 2 {
				task, ok := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).(*startTask)
				if !ok {
					t.Fatal("Start did not return an NPD task")
				}
				task.pollInterval, task.stableFor = time.Millisecond, 5*time.Millisecond
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if starts != 1 || checks < 6 {
				t.Fatalf("systemd retry was interrupted or accepted a short-lived process: starts=%d checks=%d", starts, checks)
			}
		})
	}
}

func TestNPDChangingPIDDoesNotCountAsStable(t *testing.T) {
	t.Parallel()
	checks := 0
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ns := &goalstates.NodeStart{
		MachineDir: t.TempDir(), NodeName: "worker-1",
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			if args[1] == "show" {
				checks++
				if checks == 8 {
					cancel()
				}
				return "ActiveState=active\nSubState=running\nMainPID=" + strings.Repeat("1", checks%2+1) + "\n", nil
			}
			return "active", nil
		},
	}
	task, ok := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).(*startTask)
	if !ok {
		t.Fatal("Start did not return an NPD task")
	}
	task.pollInterval, task.stableFor = time.Millisecond, 5*time.Millisecond
	if err := task.Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PID churn was accepted as stable: %v", err)
	}
	if checks != 8 {
		t.Fatalf("PID churn test did not observe all unstable samples: %d", checks)
	}
}

func TestNPDReadinessPreservesStartFailure(t *testing.T) {
	t.Parallel()
	startErr := errors.New("NPD unit could not start")
	ns := &goalstates.NodeStart{
		MachineDir: t.TempDir(), NodeName: "worker-1",
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			switch args[1] {
			case "is-active":
				return "inactive", errors.New("inactive")
			case "enable":
				return "", startErr
			case "show":
				t.Fatal("readiness polling ran after a failed start job")
			}
			return "", nil
		},
	}
	if err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(t.Context()); !errors.Is(err, startErr) {
		t.Fatalf("start failure was hidden: %v", err)
	}
}

func TestNPDReadinessCancellationAndDeadline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{name: "persistent restart loop"},
		{name: "canceled before start", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), NodeName: "worker-1",
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					calls++
					if args[1] == "show" {
						return "ActiveState=activating\nSubState=auto-restart\nMainPID=0\n", nil
					}
					return "active", nil
				},
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			want := context.DeadlineExceeded
			if tc.cancel {
				cancel()
				want = context.Canceled
			}
			err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(ctx)
			if !errors.Is(err, want) {
				t.Fatalf("error=%v, want wrapped %v", err, want)
			}
			if tc.cancel && calls != 0 {
				t.Fatalf("commands ran on a canceled start: %d", calls)
			}
		})
	}
}

func TestNPDReadinessPropagatesExecutorErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "transport", err: errors.New("status transport unavailable")},
		{name: "executor canceled", err: context.Canceled},
		{name: "executor deadline", err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			checks := 0
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), NodeName: "worker-1",
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					if args[1] == "show" {
						checks++
						return "", tc.err
					}
					return "active", nil
				},
			}
			if err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(t.Context()); !errors.Is(err, tc.err) {
				t.Fatalf("readiness hid executor error: %v", err)
			}
			if checks != 1 {
				t.Fatalf("retried a transport error: %d checks", checks)
			}
		})
	}
}

func TestNPDReadinessBoundsExecutorAndStopsOnCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checks := 0
	ns := &goalstates.NodeStart{
		MachineDir: t.TempDir(), NodeName: "worker-1",
		ExecOverride: func(probeCtx context.Context, _ *slog.Logger, args ...string) (string, error) {
			if args[1] == "show" {
				checks++
				deadline, ok := probeCtx.Deadline()
				if !ok || time.Until(deadline) > 2*time.Minute {
					t.Fatal("readiness executor has no bounded deadline")
				}
				cancel()
				<-probeCtx.Done()
				return "", probeCtx.Err()
			}
			return "active", nil
		},
	}
	if err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe returned %v", err)
	}
	if checks != 1 {
		t.Fatalf("status execution continued after cancellation: %d", checks)
	}
}
