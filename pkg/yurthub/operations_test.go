package yurthub

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

type fakeWorker struct {
	t        *testing.T
	dir      string
	commands []string
	hubState string // systemd ActiveState of the Hub service
	failNext string // a command that fails once, then succeeds
}

// run simulates commands inside the worker the way systemd behaves: enabling
// or disabling a unit whose file is missing fails, and the live kubelet
// arguments follow the routing drop-in.
func (w *fakeWorker) run(_ context.Context, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	w.commands = append(w.commands, cmd)
	if cmd == w.failNext {
		w.failNext = ""
		return "", fmt.Errorf("injected failure: %s", cmd)
	}
	unitMissing := func() bool {
		_, err := os.Stat(filepath.Join(w.dir, servicePath))
		return err != nil
	}
	switch {
	case cmd == "systemctl enable "+serviceName, cmd == "systemctl restart "+serviceName, cmd == "systemctl disable --now "+serviceName:
		if unitMissing() {
			return "", fmt.Errorf("Unit file %s does not exist", serviceName)
		}
		switch {
		case strings.Contains(cmd, "restart"):
			w.hubState = "active"
		case strings.Contains(cmd, "disable"):
			w.hubState = "inactive"
		}
		return "", nil
	case cmd == "systemctl show --property=ActiveState --value "+serviceName:
		if w.hubState == "" {
			return "inactive\n", nil
		}
		return w.hubState + "\n", nil
	case strings.HasPrefix(cmd, "systemctl show --property=MainPID"):
		return "123\n", nil
	case cmd == "cat /proc/123/cmdline":
		// Mirror unbounded's 10-kubeconfig.conf for a certificate join, and let
		// the routing drop-in replace it the way systemd would.
		args := bootstrapKubeconfigArg + " " + directKubeconfigArg
		if dropIn, err := os.ReadFile(filepath.Join(w.dir, routingDropIn)); err == nil {
			_, value, ok := strings.Cut(string(dropIn), `KUBELET_KUBECONFIG_ARGS=`)
			if !ok {
				return "", fmt.Errorf("routing drop-in sets no kubelet arguments")
			}
			args, _, _ = strings.Cut(value, `"`)
		}
		return strings.Join(append([]string{"/usr/local/bin/kubelet"}, strings.Fields(args)...), "\x00"), nil
	}
	return "", nil
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, workerCA), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFixture(t, filepath.Join(dir, directKubeconfig), directFixture)
	// Kubelet links the current client certificate with an absolute path.
	writeFixture(t, filepath.Join(dir, "var/lib/kubelet/pki/kubelet-client-2026-10-09.pem"), "client")
	if err := os.Symlink("/var/lib/kubelet/pki/kubelet-client-2026-10-09.pem", filepath.Join(dir, kubeletClientCert)); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, filepath.Join(dir, "etc/os-release"), ubuntu2404)
	return &fakeWorker{t: t, dir: dir}
}

func newTestOperations(t *testing.T, worker *fakeWorker, hubReady bool) (*Operations, *fake.Clientset, *int) {
	t.Helper()
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/readyz" && hubReady:
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/api/v1/nodes/node1" && r.Header.Get("User-Agent") == "kubelet":
			_, _ = w.Write([]byte(`{"metadata":{"name":"node1"}}`))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(hub.Close)
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node1", UID: "uid1", ResourceVersion: "1"}})
	ops, err := NewOperations(Config{
		MachineDir: worker.dir, NodeName: "node1", NodePool: "flexpool",
		ServerURL: "https://api.example.test:443", Nodes: client.CoreV1().Nodes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	ops.packages, ops.fetch = testPackages(t, []byte("fake hub binary"))
	ops.arch = "amd64"
	ops.machineRun = worker.run
	ops.healthURL = hub.URL + "/v1/readyz"
	ops.proxyBaseURL = hub.URL
	ops.readyTimeout = 200 * time.Millisecond
	ops.probeTimeout = 100 * time.Millisecond
	ops.pollInterval = 10 * time.Millisecond
	ops.removeDummy = func(context.Context) error { removed++; return nil }
	return ops, client, &removed
}

func TestOperationsEnableDisable(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, client, removed := newTestOperations(t, worker, true)
	ctx := t.Context()
	for range 2 { // every step must be safe to repeat after a crash
		for _, step := range []func(context.Context) error{ops.Prepare, ops.Route, ops.Verify} {
			if err := step(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}

	env, err := os.ReadFile(filepath.Join(worker.dir, envFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--node-name=node1", "--nodepool-name=flexpool", "--server-addr=https://api.example.test:443",
		"--node-ip=127.0.0.1", "--enable-dummy-if=false", "--bootstrap-mode=kubeletcertificate", "--discovery-token-unsafe-skip-ca-verification=false",
		"--discovery-token-ca-cert-hash=sha256:", "--enable-resource-filter=false",
		"--profiling=false",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("hub configuration missing %q:\n%s", want, env)
		}
	}
	if info, err := os.Stat(filepath.Join(worker.dir, envFile)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("hub configuration permissions: %v, %v", info, err)
	}
	for link, want := range map[string]string{hubKubeconfigLink: hubKubeconfigDest, hubCALink: hubCADest} {
		if got, err := os.Readlink(filepath.Join(worker.dir, link)); err != nil || got != want {
			t.Fatalf("link %s = %q, %v", link, got, err)
		}
	}
	node, err := client.CoreV1().Nodes().Get(ctx, "node1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if node.Annotations[durationAnnotation] != autonomyDuration || node.Annotations[completionKey] != "uid1" || node.Labels["apps.openyurt.io/nodepool"] != "flexpool" {
		t.Fatalf("metadata not initialized: %+v", node.ObjectMeta)
	}
	if _, exists := node.Annotations[legacyAutonomyAnnotation]; exists {
		t.Fatal("agent added the deprecated autonomy annotation")
	}
	// Routing must keep kubelet's TLS-bootstrap recovery.
	cmdline, err := worker.run(ctx, "cat", "/proc/123/cmdline")
	if err != nil || !slices.Equal(strings.Split(cmdline, "\x00")[1:], []string{bootstrapKubeconfigArg, proxyKubeconfigArg}) {
		t.Fatalf("routed kubelet arguments = %q, %v", cmdline, err)
	}

	for range 2 {
		for _, step := range []func(context.Context) error{ops.Restore, ops.Cleanup, ops.Stop} {
			if err := step(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got, err := os.ReadFile(filepath.Join(worker.dir, installedBinary)); err != nil || string(got) != "fake hub binary" {
		t.Fatalf("hub binary not installed: %v", err)
	}
	if *removed != 2 || !containsCommand(worker.commands, "systemctl disable --now "+serviceName) {
		t.Fatalf("stop did not disable hub and remove its interface: %v", worker.commands)
	}
	node, err = client.CoreV1().Nodes().Get(ctx, "node1", metav1.GetOptions{})
	if err != nil || node.Annotations["node.openyurt.io/autonomy-duration"] != autonomyDuration {
		t.Fatalf("disable removed customer-owned metadata: %v", err)
	}
}

func TestOperationsPrepareRejectsMissingPrerequisites(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(dir string) error{
		"unsupported image": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "etc/os-release"), []byte("ID=ubuntu\nVERSION_ID=22.04\n"), 0o644)
		},
		"no client cert":   func(dir string) error { return os.Remove(filepath.Join(dir, kubeletClientCert)) },
		"bad CA":           func(dir string) error { return os.WriteFile(filepath.Join(dir, workerCA), []byte("bad"), 0o600) },
		"conflicting file": func(dir string) error { return os.WriteFile(filepath.Join(dir, hubKubeconfigLink), []byte("x"), 0o600) },
		"conflicting link": func(dir string) error { return os.Symlink("elsewhere", filepath.Join(dir, hubCALink)) },
		"hub never ready":  func(string) error { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := newFakeWorker(t)
			if err := os.MkdirAll(filepath.Join(worker.dir, "etc/kubernetes"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := mutate(worker.dir); err != nil {
				t.Fatal(err)
			}
			ops, _, _ := newTestOperations(t, worker, name != "hub never ready")
			if err := ops.Prepare(t.Context()); err == nil {
				t.Fatal("Prepare succeeded without prerequisites")
			}
			if name != "hub never ready" && containsCommand(worker.commands, "systemctl restart "+serviceName) {
				t.Fatal("started hub before prerequisites passed")
			}
			if name == "unsupported image" {
				if _, err := os.Lstat(filepath.Join(worker.dir, hubKubeconfigLink)); !os.IsNotExist(err) {
					t.Fatal("changed the worker before reporting an unsupported image")
				}
			}
		})
	}
}

// TestOperationsDisableAfterEarlyFailure covers a worker provisioned by an
// older agent (no staged unit) that receives true, fails before staging, then
// receives false. The steps run in the engine's order.
func TestOperationsDisableAfterEarlyFailure(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(dir string) error{
		"unsupported image": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "etc/os-release"), []byte("ID=ubuntu\nVERSION_ID=22.04\n"), 0o644)
		},
		"no client cert": func(dir string) error { return os.Remove(filepath.Join(dir, kubeletClientCert)) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := newFakeWorker(t)
			if err := mutate(worker.dir); err != nil {
				t.Fatal(err)
			}
			ops, _, removed := newTestOperations(t, worker, true)
			if err := ops.Prepare(t.Context()); err == nil {
				t.Fatal("Prepare succeeded")
			}
			if _, err := os.Stat(filepath.Join(worker.dir, servicePath)); !os.IsNotExist(err) {
				t.Fatal("test setup expects the unit to be unstaged")
			}
			worker.commands = nil
			for _, step := range []func(context.Context) error{ops.Restore, ops.Cleanup, ops.Stop} {
				if err := step(t.Context()); err != nil {
					t.Fatalf("disable did not converge: %v", err)
				}
			}
			for _, cmd := range worker.commands {
				if cmd == "systemctl restart kubelet.service" || strings.HasPrefix(cmd, "systemctl disable") {
					t.Fatalf("unnecessary command during disable: %q", cmd)
				}
			}
			if *removed != 0 {
				t.Fatal("removed a hub-dummy0 this agent never configured")
			}
		})
	}
}

func TestOperationsDisableDeletesCacheKeepsPKI(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	enableForTest(t, ops)
	secret := filepath.Join(worker.dir, hubCacheDir, "kubelet/secrets.v1.core/ns/app-secret")
	writeFixture(t, secret, "cached secret")
	writeFixture(t, filepath.Join(worker.dir, servingCert), "serving cert")
	for range 2 { // idempotent
		for _, step := range []func(context.Context) error{ops.Restore, ops.Cleanup, ops.Stop} {
			if err := step(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(filepath.Join(worker.dir, hubCacheDir))
	if err != nil || len(entries) != 0 {
		t.Fatalf("hub cache not emptied: %v, %v", entries, err)
	}
	if info, err := os.Stat(filepath.Join(worker.dir, hubCacheDir)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache directory mode = %v, %v", info, err)
	}
	for _, kept := range []string{servingCert, installedBinary} {
		if _, err := os.Stat(filepath.Join(worker.dir, kept)); err != nil {
			t.Fatalf("disable removed %s: %v", kept, err)
		}
	}
}

func TestOperationsStopKeepsCacheOutsideWorker(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	outside := t.TempDir()
	writeFixture(t, filepath.Join(outside, "host-file"), "keep")
	if err := os.MkdirAll(filepath.Join(worker.dir, "var/lib/yurthub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(worker.dir, hubCacheDir)); err != nil {
		t.Fatal(err)
	}
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "host-file")); err != nil {
		t.Fatal("cache cleanup followed a symlink out of the worker")
	}
}

func TestOperationsStopRejectsRunningHubWithoutUnit(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	worker.hubState = "active"
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Stop(t.Context()); err == nil {
		t.Fatal("treated a running hub without its unit as stopped")
	}
}

func countCommand(commands []string, want string) int {
	n := 0
	for _, cmd := range commands {
		if cmd == want {
			n++
		}
	}
	return n
}

func TestOperationsConverge(t *testing.T) {
	t.Parallel()
	restart := "systemctl restart " + serviceName
	for name, mutate := range map[string]func(t *testing.T, ops *Operations, dir string){
		"no change": func(*testing.T, *Operations, string) {},
		"new binary in agent": func(t *testing.T, ops *Operations, _ string) {
			ops.packages, ops.fetch = testPackages(t, []byte("newer hub binary"))
		},
		"changed arguments": func(t *testing.T, _ *Operations, dir string) {
			if err := os.WriteFile(filepath.Join(dir, envFile), []byte("YURTHUB_ARGS=\"--node-ip=10.0.0.4\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"changed unit": func(t *testing.T, _ *Operations, dir string) {
			if err := os.WriteFile(filepath.Join(dir, servicePath), []byte("[Service]\nExecStart=/old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"no package for worker": func(_ *testing.T, ops *Operations, _ string) {
			ops.packages = nil
		},
		"new package unreachable": func(t *testing.T, ops *Operations, _ string) {
			ops.packages, _ = testPackages(t, []byte("newer hub binary"))
			ops.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := newFakeWorker(t)
			ops, _, _ := newTestOperations(t, worker, true)
			for _, step := range []func(context.Context) error{ops.Prepare, ops.Route, ops.Verify} {
				if err := step(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			mutate(t, ops, worker.dir)
			worker.commands = nil
			for range 2 {
				if err := ops.Converge(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			wantRestarts := 1
			if name == "no change" || name == "no package for worker" || name == "new package unreachable" {
				wantRestarts = 0
			}
			if got := countCommand(worker.commands, restart); got != wantRestarts {
				t.Fatalf("hub restarts = %d, want %d: %v", got, wantRestarts, worker.commands)
			}
			if containsCommand(worker.commands, "systemctl restart kubelet.service") {
				t.Fatal("converge restarted kubelet")
			}
			env, err := os.ReadFile(filepath.Join(worker.dir, envFile))
			if err != nil || !strings.Contains(string(env), "--node-ip=127.0.0.1") {
				t.Fatalf("arguments not converged: %s, %v", env, err)
			}
			unit, err := os.ReadFile(filepath.Join(worker.dir, servicePath))
			if err != nil || !bytes.Equal(unit, serviceUnit) {
				t.Fatalf("unit not converged: %v", err)
			}
			want := "fake hub binary"
			if name == "new binary in agent" {
				want = "newer hub binary"
			}
			if got, err := os.ReadFile(filepath.Join(worker.dir, installedBinary)); err != nil || string(got) != want {
				t.Fatalf("binary = %q, %v; want %q", got, err, want)
			}
		})
	}
}

func enableForTest(t *testing.T, ops *Operations) {
	t.Helper()
	for _, step := range []func(context.Context) error{ops.Prepare, ops.Route, ops.Verify} {
		if err := step(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestOperationsConvergeRetriesOwedRestart covers a restart that fails after the
// files were rewritten: the next read must still restart, not see "no change".
func TestOperationsConvergeRetriesOwedRestart(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	enableForTest(t, ops)
	ops.packages, ops.fetch = testPackages(t, []byte("newer hub binary"))
	restart := "systemctl restart " + serviceName
	worker.failNext = restart
	worker.commands = nil
	if err := ops.Converge(t.Context()); err == nil {
		t.Fatal("Converge ignored a failed restart")
	}
	worker.commands = nil
	if err := ops.Converge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := countCommand(worker.commands, restart); got != 1 {
		t.Fatalf("owed restart not retried: %v", worker.commands)
	}
	worker.commands = nil
	if err := ops.Converge(t.Context()); err != nil || countCommand(worker.commands, restart) != 0 {
		t.Fatalf("restarted again after a successful restart: %v, %v", worker.commands, err)
	}
}

// TestOperationsDriftRepairWithFailedRestart: Hub was started with files A,
// then files changed to B and Hub was restarted outside the agent. Converge
// restores A; if that restart fails, the next read must still restart rather
// than see A matching the old record.
func TestOperationsDriftRepairWithFailedRestart(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	enableForTest(t, ops)
	if err := os.WriteFile(filepath.Join(worker.dir, envFile), []byte("YURTHUB_ARGS=\"--v=9\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restart := "systemctl restart " + serviceName
	worker.failNext = restart
	if err := ops.Converge(t.Context()); err == nil {
		t.Fatal("Converge ignored a failed restart")
	}
	worker.commands = nil
	if err := ops.Converge(t.Context()); err != nil {
		t.Fatal(err)
	}
	if countCommand(worker.commands, restart) != 1 {
		t.Fatalf("restart after drift repair not retried: %v", worker.commands)
	}
}

// TestOperationsConvergeKeepsHubWhenRepairFails: the installed binary is
// removed or corrupted while PMC is unreachable. The running Hub still serves
// from the old file, so Converge must not restart it until a repair succeeds.
func TestOperationsConvergeKeepsHubWhenRepairFails(t *testing.T) {
	t.Parallel()
	restart := "systemctl restart " + serviceName
	for name, damage := range map[string]func(path string) error{
		"missing binary": os.Remove,
		"corrupt binary": func(path string) error { return os.WriteFile(path, []byte("corrupt"), 0o755) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			worker := newFakeWorker(t)
			ops, _, _ := newTestOperations(t, worker, true)
			enableForTest(t, ops)
			fetch := ops.fetch
			ops.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }
			if err := damage(filepath.Join(worker.dir, installedBinary)); err != nil {
				t.Fatal(err)
			}
			worker.commands = nil
			for range 2 {
				if err := ops.Converge(t.Context()); !errors.Is(err, ErrPackageDownload) {
					t.Fatalf("Converge error = %v, want ErrPackageDownload", err)
				}
			}
			if got := countCommand(worker.commands, restart); got != 0 {
				t.Fatalf("restarted hub without a repaired binary: %v", worker.commands)
			}
			ops.fetch = fetch
			worker.commands = nil
			if err := ops.Converge(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := countCommand(worker.commands, restart); got != 1 {
				t.Fatalf("hub restarts after repair = %d, want 1: %v", got, worker.commands)
			}
		})
	}
}

// TestOperationsRouteConvergesResumedEnable: an older agent finished Prepare,
// a newer agent resumes at Route. Hub must run the new files before kubelet
// is routed to it.
func TestOperationsRouteConvergesResumedEnable(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	ops.packages, ops.fetch = testPackages(t, []byte("newer hub binary"))
	worker.commands = nil
	if err := ops.Route(t.Context()); err != nil {
		t.Fatal(err)
	}
	hubRestart, kubeletRestart := -1, -1
	for i, cmd := range worker.commands {
		switch cmd {
		case "systemctl restart " + serviceName:
			hubRestart = i
		case "systemctl restart kubelet.service":
			kubeletRestart = i
		}
	}
	if hubRestart < 0 || kubeletRestart < 0 || hubRestart > kubeletRestart {
		t.Fatalf("hub not updated before routing kubelet: %v", worker.commands)
	}
	if got, err := os.ReadFile(filepath.Join(worker.dir, installedBinary)); err != nil || string(got) != "newer hub binary" {
		t.Fatalf("binary = %q, %v", got, err)
	}
}

// blockingNodes stalls Get until the caller's context ends.
type blockingNodes struct{ typedcorev1.NodeInterface }

func (blockingNodes) Get(ctx context.Context, _ string, _ metav1.GetOptions) (*corev1.Node, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestOperationsVerifyBoundsNodeAPI(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, client, _ := newTestOperations(t, worker, true)
	if err := ops.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ops.Route(t.Context()); err != nil {
		t.Fatal(err)
	}
	ops.cfg.Nodes = blockingNodes{client.CoreV1().Nodes()}
	ops.nodeAPITimeout = 100 * time.Millisecond
	start := time.Now()
	if err := ops.Verify(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Verify error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("stalled Node API held Verify for %v", elapsed)
	}
}

func TestOperationsConvergeReportsUnreadyHub(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	enableForTest(t, ops)
	ops.healthURL = ops.proxyBaseURL + "/down"
	for range 2 { // reported on every read, not only the first
		if err := ops.Converge(t.Context()); err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("Converge error = %v, want not ready", err)
		}
	}
}

// TestOperationsPrepareRestartsRunningHub covers a Prepare retry while Hub is
// already running (its CSR was pending) after an agent upgrade changed files.
func TestOperationsPrepareRestartsRunningHub(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	restart := "systemctl restart " + serviceName
	if err := ops.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker.commands = nil
	if err := ops.Prepare(t.Context()); err != nil || countCommand(worker.commands, restart) != 0 {
		t.Fatalf("unchanged Prepare retry restarted a running hub: %v, %v", worker.commands, err)
	}
	ops.packages, ops.fetch = testPackages(t, []byte("newer hub binary"))
	worker.commands = nil
	if err := ops.Prepare(t.Context()); err != nil || countCommand(worker.commands, restart) != 1 {
		t.Fatalf("running hub not restarted with new binary: %v, %v", worker.commands, err)
	}
}

func TestOperationsPrepareExecCredentialJoinIsUnsupported(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	if err := os.Remove(filepath.Join(worker.dir, kubeletClientCert)); err != nil {
		t.Fatal(err)
	}
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Prepare(t.Context()); !errors.Is(err, ErrUnsupportedWorker) {
		t.Fatalf("Prepare error = %v, want ErrUnsupportedWorker", err)
	}
	if _, err := os.Stat(filepath.Join(worker.dir, installedBinary)); !os.IsNotExist(err) {
		t.Fatal("installed the hub binary on an unsupported worker")
	}
}

func TestOperationsPrepareReportsDownloadFailure(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	ops.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("offline") }
	if err := ops.Prepare(t.Context()); !errors.Is(err, ErrPackageDownload) {
		t.Fatalf("Prepare error = %v, want ErrPackageDownload", err)
	}
	if containsCommand(worker.commands, "systemctl restart "+serviceName) {
		t.Fatal("started hub without a binary")
	}
}

func TestOperationsRouteRequiresReadyHub(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	ops.healthURL = ops.proxyBaseURL + "/down"
	if err := ops.Route(t.Context()); err == nil {
		t.Fatal("routed kubelet to a hub that is not ready")
	}
	if _, err := os.Stat(filepath.Join(worker.dir, routingDropIn)); !os.IsNotExist(err) {
		t.Fatal("wrote the routing drop-in before hub was ready")
	}
}

func TestOperationsReadinessErrorNamesPendingCSR(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, false)
	err := ops.Prepare(t.Context())
	if err == nil || !strings.Contains(err.Error(), "CSR is not approved") {
		t.Fatalf("Prepare error = %v, want pending CSR message", err)
	}
	writeFixture(t, filepath.Join(worker.dir, servingCert), "cert")
	err = ops.Prepare(t.Context())
	if err == nil || strings.Contains(err.Error(), "CSR") {
		t.Fatalf("Prepare error = %v, want a non-CSR readiness message", err)
	}
}

func TestOperationsRestoreSkipsRestartWhenNeverRouted(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, _, _ := newTestOperations(t, worker, true)
	if err := ops.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if containsCommand(worker.commands, "systemctl restart kubelet.service") {
		t.Fatal("restarted kubelet although routing was never applied")
	}
}

func TestOperationsVerifyRequiresLiveRouting(t *testing.T) {
	t.Parallel()
	worker := newFakeWorker(t)
	ops, client, _ := newTestOperations(t, worker, true)
	if err := ops.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Routing never configured, so the live kubelet still uses the direct API.
	if err := ops.Verify(t.Context()); err == nil {
		t.Fatal("Verify passed while kubelet bypassed hub")
	}
	node, err := client.CoreV1().Nodes().Get(t.Context(), "node1", metav1.GetOptions{})
	if err != nil || node.Annotations[completionKey] != "" {
		t.Fatal("metadata initialized before routing verified")
	}
}

func TestNewOperationsValidatesInputs(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	base := Config{MachineDir: "/var/lib/machines/kube1", NodeName: "node1", NodePool: "flexpool", ServerURL: "https://api.example.test:443", Nodes: client.CoreV1().Nodes()}
	for name, mutate := range map[string]func(*Config){
		"relative dir":   func(c *Config) { c.MachineDir = "kube1" },
		"no client":      func(c *Config) { c.Nodes = nil },
		"bad node":       func(c *Config) { c.NodeName = "Node 1" },
		"bad pool":       func(c *Config) { c.NodePool = "pool_1" },
		"http server":    func(c *Config) { c.ServerURL = "http://api.example.test" },
		"quoted server":  func(c *Config) { c.ServerURL = `https://api.example.test/" --v=10` },
		"user in server": func(c *Config) { c.ServerURL = "https://u:p@api.example.test" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := base
			mutate(&cfg)
			if _, err := NewOperations(cfg); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := NewOperations(base); err != nil {
		t.Fatal(err)
	}
}

func containsCommand(commands []string, want string) bool {
	for _, cmd := range commands {
		if cmd == want {
			return true
		}
	}
	return false
}
