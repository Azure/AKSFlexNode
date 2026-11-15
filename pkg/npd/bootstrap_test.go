package npd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestNPDWaitsForTLSBootstrapCertificate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"nspawn", "none", "container"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			var calls []string
			certificateChecks := 0
			credentialsReady, npdActive := false, false
			ns := &goalstates.NodeStart{
				MachineDir: root, NodeName: "worker-1",
				Kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "do-not-log-token"}},
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					calls = append(calls, strings.Join(args, " "))
					if args[0] == "test" {
						if len(args) != 3 || args[1] != "-s" {
							t.Fatalf("unexpected credential probe: %v", args)
						}
						switch args[2] {
						case goalstates.KubeletKubeconfigPath:
							// Kubelet has already written a kubeconfig referencing
							// the not-yet-published client PEM, as in the CI failure.
							return "", nil
						case kubeletClientCertificatePath:
							certificateChecks++
							if certificateChecks == 1 {
								return "", errors.New("client certificate has not been published")
							}
							credentialsReady = true
							return "", nil
						default:
							t.Fatalf("credential probe escaped the node filesystem: %v", args)
						}
					}
					if !credentialsReady {
						t.Fatalf("NPD systemd action occurred before credentials were ready: %v", args)
					}
					if args[1] == "is-active" && !npdActive {
						return "inactive", errors.New("NPD is not started")
					}
					if args[1] == "enable" || args[1] == "restart" {
						npdActive = true
					}
					if args[1] == "show" {
						return runningNPDStatus, nil
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
			task, ok := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).(*startTask)
			if !ok {
				t.Fatal("Start did not return an NPD startup task")
			}
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if certificateChecks < 3 {
				t.Fatalf("certificate was not checked on initial start and retry: %v", calls)
			}
			if calls[len(calls)-1] != npdStatusCommand(task.unit) {
				t.Fatalf("startup did not retain the running-process check: %v", calls)
			}
			for _, call := range calls {
				if strings.Contains(call, "do-not-log-token") {
					t.Fatal("credential probe included a bootstrap token")
				}
			}
		})
	}
}

func TestNPDDoesNotRequireBootstrapPEMForDirectCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		kubelet goalstates.Kubelet
	}{
		{name: "exec plugin", kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{ExecCredential: &clientcmdapi.ExecConfig{Command: "credential-plugin"}}}},
		{name: "embedded kubeconfig", kubelet: goalstates.Kubelet{KubeconfigData: []byte("embedded config")}},
		{name: "exec takes precedence over bootstrap token", kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "unused", ExecCredential: &clientcmdapi.ExecConfig{Command: "credential-plugin"}}}},
		{name: "embedded takes precedence over bootstrap token", kubelet: goalstates.Kubelet{KubeconfigData: []byte("embedded config"), KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "unused"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), NodeName: "worker-1", Kubelet: tc.kubelet,
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					if args[0] != "systemctl" {
						t.Fatalf("direct credentials unexpectedly required a bootstrap PEM: %v", args)
					}
					if args[1] == "show" {
						return runningNPDStatus, nil
					}
					return "active", nil
				},
			}
			if err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNPDWaitForCredentialsRespectsContext(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		missing  string
		cancel   bool
		probeErr error
		want     error
	}{
		{name: "missing kubeconfig", missing: goalstates.KubeletKubeconfigPath, want: context.DeadlineExceeded},
		{name: "missing certificate", missing: kubeletClientCertificatePath, want: context.DeadlineExceeded},
		{name: "canceled context", missing: kubeletClientCertificatePath, cancel: true, want: context.Canceled},
		{name: "executor canceled", missing: kubeletClientCertificatePath, probeErr: context.Canceled, want: context.Canceled},
		{name: "executor timed out", missing: kubeletClientCertificatePath, probeErr: context.DeadlineExceeded, want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns := &goalstates.NodeStart{
				MachineDir: t.TempDir(), NodeName: "worker-1",
				Kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "do-not-log-token"}},
				ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
					if args[0] != "test" {
						t.Fatalf("NPD action ran without ready credentials: %v", args)
					}
					if args[2] == tc.missing {
						if tc.probeErr != nil {
							return "", tc.probeErr
						}
						return "", errors.New("not ready")
					}
					return "", nil
				},
			}
			timeout := 10 * time.Millisecond
			if tc.probeErr != nil {
				timeout = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(ctx)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want wrapped %v", err, tc.want)
			}
			if strings.Contains(err.Error(), "do-not-log-token") {
				t.Fatal("credential wait returned a bootstrap token")
			}
		})
	}
}

func TestNPDWaitFailurePreservesPendingServiceUpdate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	var reloads, restarts, statusChecks int
	ns := &goalstates.NodeStart{
		MachineDir: root, NodeName: "old-worker",
		Kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "do-not-log-token"}},
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			if args[0] == "test" {
				return "", nil
			}
			switch args[1] {
			case "daemon-reload":
				reloads++
			case "restart":
				restarts++
			case "show":
				statusChecks++
				return runningNPDStatus, nil
			}
			return "active", nil
		},
	}
	old, ok := Start(log, &config.Config{}, ns).(*startTask)
	if !ok {
		t.Fatal("Start did not return an NPD task")
	}
	if _, err := old.ensureServiceFile(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "etc/systemd/system", systemdUnitNPD)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ns.NodeName = "new-worker"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Start(log, &config.Config{}, ns).Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait returned %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || reloads != 0 || restarts != 0 || statusChecks != 0 {
		t.Fatalf("failed wait published a pending service update: %v", err)
	}
	for range 2 {
		if err := Start(log, &config.Config{}, ns).Do(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if reloads != 1 || restarts != 1 || statusChecks != 2 {
		t.Fatalf("retry lost or repeated the service update: reloads=%d restarts=%d checks=%d", reloads, restarts, statusChecks)
	}
	if strings.Contains(logs.String(), "do-not-log-token") {
		t.Fatal("credential wait logged a bootstrap token")
	}
}

func TestNPDWaitBoundsExecutorContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	ns := &goalstates.NodeStart{
		MachineDir: t.TempDir(), NodeName: "worker-1",
		Kubelet: goalstates.Kubelet{KubeletAuthInfo: agentconfig.KubeletAuthInfo{BootstrapToken: "do-not-log-token"}},
		ExecOverride: func(probeCtx context.Context, _ *slog.Logger, args ...string) (string, error) {
			calls++
			deadline, ok := probeCtx.Deadline()
			if !ok || time.Until(deadline) > 2*time.Minute {
				t.Fatal("credential executor has no bounded bootstrap deadline")
			}
			cancel()
			<-probeCtx.Done()
			return "", probeCtx.Err()
		},
	}
	if err := Start(slog.New(slog.DiscardHandler), &config.Config{}, ns).Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled probe returned %v", err)
	}
	if calls != 1 {
		t.Fatalf("execution continued after cancellation: %d calls", calls)
	}
}
