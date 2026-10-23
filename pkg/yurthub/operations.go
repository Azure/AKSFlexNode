package yurthub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const (
	serviceName = "aks-flex-yurthub.service"
	envFile     = "etc/aks-flex-node/yurthub.env"

	// Hub's kubeletcertificate mode reads these fixed paths. Unbounded writes the
	// same material elsewhere, so the agent adds relative links inside the worker.
	hubKubeconfigLink = "etc/kubernetes/kubelet.conf"
	hubKubeconfigDest = "../../var/lib/kubelet/kubeconfig"
	hubCALink         = "etc/kubernetes/pki/ca.crt"
	hubCADest         = "apiserver-client-ca.crt"
	workerCA          = "etc/kubernetes/pki/apiserver-client-ca.crt"
	kubeletClientCert = "var/lib/kubelet/pki/kubelet-client-current.pem"
	servingCert       = "var/lib/yurthub/pki/yurthub-server-current.pem"
	hubCacheDir       = "var/lib/yurthub/cache"
	// startedFingerprint records the files the agent last started Hub with.
	startedFingerprint = "var/lib/aks-flex-node/hub-started.sha256"

	// The worker shares the host network namespace, so Hub's loopback listeners
	// and dummy interface are reachable and visible from the host agent.
	hubHealthURL = "http://127.0.0.1:10267/v1/readyz"
	hubDummyIf   = "hub-dummy0"
)

// noResourceFilters makes Hub serve kubelet only. With filters on, pods started
// under Hub keep calling Hub and lose API access when it's disabled. See
// docs/design/yurthub-agent-integration.md.
const noResourceFilters = "--enable-resource-filter=false"

// loopbackOnlyArgs keep every Hub listener on 127.0.0.1, because Hub forwards
// unauthenticated requests with the node's kubelet certificate. See
// docs/design/yurthub-agent-integration.md.
var loopbackOnlyArgs = []string{"--node-ip=127.0.0.1", "--enable-dummy-if=false"}

// Config identifies the worker and node a set of Hub operations acts on.
type Config struct {
	Log        *slog.Logger
	MachineDir string // host path of the worker rootfs, e.g. /var/lib/machines/kube1
	NodeName   string
	NodePool   string // Flex agent pool name, used as the apps.openyurt.io/nodepool label
	ServerURL  string // direct HTTPS API server URL
	Nodes      typedcorev1.NodeInterface
}

// Operations implements the daemon's Hub transition steps for one worker. Each
// step is idempotent because the engine can repeat a step after a crash.
type Operations struct {
	cfg            Config
	machine        string
	machineRun     func(ctx context.Context, args ...string) (string, error)
	httpClient     *http.Client
	healthURL      string
	proxyBaseURL   string
	removeDummy    func(ctx context.Context) error
	packages       []hubPackage
	fetch          func(ctx context.Context, url string) ([]byte, error)
	arch           string
	readyTimeout   time.Duration
	probeTimeout   time.Duration
	nodeAPITimeout time.Duration
	pollInterval   time.Duration
}

// NewOperations validates inputs once so no step can render unsafe arguments.
func NewOperations(cfg Config) (*Operations, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if !filepath.IsAbs(cfg.MachineDir) || cfg.Nodes == nil {
		return nil, fmt.Errorf("hub operations require an absolute worker path and node client")
	}
	if len(validation.IsDNS1123Subdomain(cfg.NodeName)) != 0 {
		return nil, fmt.Errorf("invalid node name for hub")
	}
	if len(validation.IsDNS1123Label(cfg.NodePool)) != 0 {
		return nil, fmt.Errorf("invalid agent pool name for hub NodePool")
	}
	server, err := url.Parse(cfg.ServerURL)
	if err != nil || server.Scheme != "https" || server.Host == "" || server.User != nil || strings.ContainsAny(cfg.ServerURL, " \t\n\"'\\$") {
		return nil, fmt.Errorf("hub requires a plain HTTPS API server URL")
	}
	packages, err := loadPackages()
	if err != nil {
		return nil, err
	}
	machine := filepath.Base(cfg.MachineDir)
	return &Operations{
		cfg:     cfg,
		machine: machine,
		machineRun: func(ctx context.Context, args ...string) (string, error) {
			return utilexec.MachineRun(ctx, cfg.Log, machine, args...)
		},
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		healthURL:    hubHealthURL,
		proxyBaseURL: proxyURL,
		removeDummy: func(ctx context.Context) error {
			return removeDummyInterface(ctx, cfg.Log)
		},
		packages:       packages,
		fetch:          downloadPackage,
		arch:           runtime.GOARCH,
		readyTimeout:   2 * time.Minute,
		probeTimeout:   15 * time.Second,
		nodeAPITimeout: time.Minute,
		pollInterval:   2 * time.Second,
	}, nil
}

// Prepare checks local prerequisites, renders configuration and starts Hub
// without touching kubelet routing. If Hub is already running (for example a
// retry while its serving CSR is pending), it is restarted only when the files
// it would load differ from what the agent last started it with.
func (o *Operations) Prepare(ctx context.Context) error {
	root, err := os.OpenRoot(o.cfg.MachineDir)
	if err != nil {
		return fmt.Errorf("open worker rootfs: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	// Report unsupported workers before anything in the worker changes. Hub's
	// kubeletcertificate mode needs kubelet's client certificate, which joins
	// that use only an exec credential (service principal, MSI, Arc) don't have.
	if !workerFileExists(root, kubeletClientCert) {
		return fmt.Errorf("%w: kubelet has no client certificate (exec-credential join)", ErrUnsupportedWorker)
	}
	if err := invalidateDriftedStart(root); err != nil {
		return err
	}
	version, err := installBinary(ctx, root, o.packages, o.arch, o.fetch)
	if err != nil {
		return err
	}
	o.cfg.Log.Info("hub binary ready in worker", "machine", o.machine, "package", version)
	for _, link := range [][2]string{{hubKubeconfigLink, hubKubeconfigDest}, {hubCALink, hubCADest}} {
		if err := ensureLink(root, link[0], link[1]); err != nil {
			return err
		}
	}
	if err := ensureStateDirs(root); err != nil {
		return err
	}
	if err := stageService(root); err != nil {
		return err
	}
	if err := o.ensureEnv(root); err != nil {
		return err
	}
	if _, err := o.machineRun(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload worker systemd: %w", err)
	}
	if _, err := o.machineRun(ctx, "systemctl", "enable", serviceName); err != nil {
		return fmt.Errorf("enable hub: %w", err)
	}
	state, err := o.machineRun(ctx, "systemctl", "show", "--property=ActiveState", "--value", serviceName)
	if err != nil {
		return fmt.Errorf("check hub state: %w", err)
	}
	running := strings.TrimSpace(state) == "active" || strings.TrimSpace(state) == "activating"
	if !running || !startedWithCurrentFiles(root) {
		if err := o.restartHub(ctx, root); err != nil {
			return err
		}
	}
	// Readiness includes Hub's serving certificate, so an unapproved serving CSR
	// surfaces here. Bounded because the daemon queue is serialized.
	return o.waitReadyFor(ctx, o.readyTimeout)
}

// Converge brings an enabled Hub's binary, arguments and unit up to this agent
// and restarts Hub only if they differ from what it was last started with;
// otherwise it checks readiness. It never touches kubelet or Node metadata.
func (o *Operations) Converge(ctx context.Context) error {
	root, err := os.OpenRoot(o.cfg.MachineDir)
	if err != nil {
		return fmt.Errorf("open worker rootfs: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	if err := invalidateDriftedStart(root); err != nil {
		return err
	}
	version, err := installBinary(ctx, root, o.packages, o.arch, o.fetch)
	if errors.Is(err, ErrUnsupportedWorker) || errors.Is(err, ErrPackageDownload) {
		// Without a start record matching the files on disk, the installed
		// binary may be missing or corrupt. Restarting would stop a Hub that
		// still runs from the old file, so wait for a successful repair.
		if !startedWithCurrentFiles(root) {
			return fmt.Errorf("hub files changed and the binary could not be repaired: %w", err)
		}
		o.cfg.Log.Warn("keeping the installed hub binary", "machine", o.machine, "reason", err.Error())
	} else if err != nil {
		return err
	}
	if err := o.ensureEnv(root); err != nil {
		return err
	}
	if err := stageService(root); err != nil {
		return err
	}
	if startedWithCurrentFiles(root) {
		if err := o.waitReadyFor(ctx, o.probeTimeout); err != nil {
			return fmt.Errorf("enabled hub is not ready: %w", err)
		}
		return nil
	}
	o.cfg.Log.Info("updating enabled hub", "machine", o.machine, "package", version)
	if err := o.restartHub(ctx, root); err != nil {
		return err
	}
	return o.waitReadyFor(ctx, o.readyTimeout)
}

// restartHub reloads systemd, restarts Hub (starting it if stopped) and records
// the fingerprint of the files Hub was started with. The record is written only
// after the restart command succeeds, so a failed or interrupted restart is
// retried on the next read.
func (o *Operations) restartHub(ctx context.Context, root *os.Root) error {
	if _, err := o.machineRun(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload worker systemd: %w", err)
	}
	if _, err := o.machineRun(ctx, "systemctl", "restart", serviceName); err != nil {
		return fmt.Errorf("restart hub: %w", err)
	}
	if err := utilio.WriteFileInRoot(root, startedFingerprint, []byte(filesFingerprint(root)+"\n"), 0o600); err != nil {
		return fmt.Errorf("record hub start: %w", err)
	}
	return nil
}

// invalidateDriftedStart drops the start record when the files on disk no
// longer match it, before the agent repairs them. Otherwise a repair back to
// the recorded files followed by a failed restart would look current on the
// next read while Hub still runs the drifted files.
func invalidateDriftedStart(root *os.Root) error {
	if _, err := root.Lstat(startedFingerprint); errors.Is(err, os.ErrNotExist) || startedWithCurrentFiles(root) {
		return nil
	}
	if err := root.Remove(startedFingerprint); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear hub start record: %w", err)
	}
	return nil
}

// startedWithCurrentFiles reports whether Hub was last started by the agent
// with the binary, arguments and unit currently on disk.
func startedWithCurrentFiles(root *os.Root) bool {
	recorded, err := root.ReadFile(startedFingerprint)
	return err == nil && strings.TrimSpace(string(recorded)) == filesFingerprint(root)
}

// filesFingerprint hashes what a Hub start loads: binary, arguments and unit.
func filesFingerprint(root *os.Root) string {
	h := sha256.New()
	h.Write([]byte(installedSHA256(root)))
	for _, name := range []string{envFile, servicePath} {
		data, _ := root.ReadFile(name)
		h.Write([]byte{0})
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ensureEnv writes Hub's arguments when they differ. The CA hash is recomputed so
// a rotated API server CA converges too.
func (o *Operations) ensureEnv(root *os.Root) error {
	caHash, err := caPublicKeyHash(root)
	if err != nil {
		return err
	}
	want := o.envContent(caHash)
	if current, err := root.ReadFile(envFile); err == nil && bytes.Equal(current, want) {
		return nil
	}
	if err := utilio.WriteFileInRoot(root, envFile, want, 0o600); err != nil {
		return fmt.Errorf("write hub configuration: %w", err)
	}
	return nil
}

// Route switches kubelet to Hub. The direct kubeconfig stays in place. It first
// converges Hub's files and checks Hub is ready and forwards a kubelet request:
// a crash, retry or agent upgrade can resume at Route long after Prepare ran.
func (o *Operations) Route(ctx context.Context) error {
	if err := o.Converge(ctx); err != nil {
		return fmt.Errorf("hub is not ready before routing kubelet: %w", err)
	}
	if err := o.getNodeThroughHub(ctx); err != nil {
		return fmt.Errorf("hub cannot forward before routing kubelet: %w", err)
	}
	if err := configureRouting(o.cfg.MachineDir); err != nil {
		return err
	}
	return o.restartKubelet(ctx)
}

// Verify checks live routing and upstream access through Hub, then initializes
// Node metadata once. It is a connected check, not an initial-sync Ready gate.
func (o *Operations) Verify(ctx context.Context) error {
	// Converge first so a Verify resumed by a newer agent runs current files.
	if err := o.Converge(ctx); err != nil {
		return err
	}
	if err := o.expectKubeletArg(ctx, proxyKubeconfigArg); err != nil {
		return err
	}
	if err := o.getNodeThroughHub(ctx); err != nil {
		return err
	}
	// Bound the direct API calls: Verify runs on the serialized daemon queue,
	// so a stalled response must not block repave, reset or agent upgrade.
	ctx, cancel := context.WithTimeout(ctx, o.nodeAPITimeout)
	defer cancel()
	node, err := o.cfg.Nodes.Get(ctx, o.cfg.NodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get node for hub metadata: %w", err)
	}
	return initializeMetadata(ctx, o.cfg.Nodes, o.cfg.NodeName, node.UID, o.cfg.NodePool)
}

// Restore returns kubelet to the direct API server and proves the live process
// uses it before anything stops Hub.
func (o *Operations) Restore(ctx context.Context) error {
	removed, err := restoreRouting(o.cfg.MachineDir)
	if err != nil {
		return err
	}
	// Skip the kubelet restart when routing was never applied (for example an
	// unsupported worker, or a repeat after a completed restore).
	if !removed && o.expectKubeletArg(ctx, directKubeconfigArg) == nil {
		return nil
	}
	if err := o.restartKubelet(ctx); err != nil {
		return err
	}
	return o.expectKubeletArg(ctx, directKubeconfigArg)
}

// Cleanup has nothing to undo: with filters off, Hub never changed what pods see.
func (o *Operations) Cleanup(context.Context) error { return nil }

// Stop disables Hub, deletes its cache and removes a dummy interface left by an
// older agent. A missing unit (enable failed before staging) counts as stopped
// if systemd agrees. The interface is removed only if this agent configured Hub.
func (o *Operations) Stop(ctx context.Context) error {
	root, err := os.OpenRoot(o.cfg.MachineDir)
	if err != nil {
		return fmt.Errorf("open worker rootfs: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	if _, err := root.Lstat(servicePath); err == nil {
		if _, err := o.machineRun(ctx, "systemctl", "disable", "--now", serviceName); err != nil {
			return fmt.Errorf("stop hub: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect hub unit: %w", err)
	} else {
		state, err := o.machineRun(ctx, "systemctl", "show", "--property=ActiveState", "--value", serviceName)
		if err != nil {
			return fmt.Errorf("check hub state: %w", err)
		}
		if state = strings.TrimSpace(state); state != "inactive" && state != "failed" {
			return fmt.Errorf("hub unit file is missing but the service is %q", state)
		}
	}
	if err := root.Remove(startedFingerprint); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear hub start record: %w", err)
	}
	// The cache holds plaintext Secrets for pods on the node. PKI and the binary
	// stay so re-enable needs no new serving CSR.
	if err := root.RemoveAll(hubCacheDir); err != nil {
		return fmt.Errorf("remove hub cache: %w", err)
	}
	if err := root.MkdirAll(hubCacheDir, 0o700); err != nil {
		return fmt.Errorf("recreate hub cache directory: %w", err)
	}
	if _, err := root.Lstat(envFile); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect hub configuration: %w", err)
	}
	return o.removeDummy(ctx)
}

func (o *Operations) envContent(caHash string) []byte {
	args := []string{
		"--node-name=" + o.cfg.NodeName,
		"--nodepool-name=" + o.cfg.NodePool,
		"--server-addr=" + o.cfg.ServerURL,
		"--bootstrap-mode=kubeletcertificate",
		"--discovery-token-unsafe-skip-ca-verification=false",
		"--discovery-token-ca-cert-hash=sha256:" + caHash,
		noResourceFilters,
		"--profiling=false",
		"--v=2",
	}
	args = append(args, loopbackOnlyArgs...)
	return []byte(`YURTHUB_ARGS="` + strings.Join(args, " ") + "\"\n")
}

func (o *Operations) restartKubelet(ctx context.Context) error {
	if _, err := o.machineRun(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("reload worker systemd: %w", err)
	}
	if _, err := o.machineRun(ctx, "systemctl", "restart", "kubelet.service"); err != nil {
		return fmt.Errorf("restart kubelet: %w", err)
	}
	return nil
}

// expectKubeletArg checks the running kubelet, not just files on disk.
func (o *Operations) expectKubeletArg(ctx context.Context, want string) error {
	pid, err := o.machineRun(ctx, "systemctl", "show", "--property=MainPID", "--value", "kubelet.service")
	pid = strings.TrimSpace(pid)
	if err != nil || pid == "" || pid == "0" || strings.ContainsAny(pid, "/ .") {
		return fmt.Errorf("kubelet is not running")
	}
	cmdline, err := o.machineRun(ctx, "cat", "/proc/"+pid+"/cmdline")
	if err != nil {
		return fmt.Errorf("read kubelet arguments: %w", err)
	}
	var kubeconfigs []string
	for _, arg := range strings.Split(cmdline, "\x00") {
		if strings.HasPrefix(arg, "--kubeconfig=") {
			kubeconfigs = append(kubeconfigs, arg)
		}
	}
	if len(kubeconfigs) != 1 || kubeconfigs[0] != want {
		return fmt.Errorf("kubelet is not using the expected API route")
	}
	return nil
}

func (o *Operations) waitReadyFor(parent context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	for {
		if o.get(ctx, o.healthURL, "", nil) == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			// Report agent shutdown as such, not as a cluster problem.
			if err := parent.Err(); err != nil {
				return err
			}
			if !o.hasServingCert() {
				return fmt.Errorf("hub is waiting for its serving certificate; its kubelet-serving CSR is not approved yet")
			}
			return fmt.Errorf("hub did not become ready (check cluster prerequisites: yurt-hub-cfg ConfigMap access)")
		case <-time.After(o.pollInterval):
		}
	}
}

// hasServingCert reports whether Hub has written its approved serving cert.
func (o *Operations) hasServingCert() bool {
	root, err := os.OpenRoot(o.cfg.MachineDir)
	if err != nil {
		return false
	}
	defer root.Close() //nolint:errcheck // directory handle
	return workerFileExists(root, servingCert)
}

func (o *Operations) getNodeThroughHub(ctx context.Context) error {
	var node struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := o.get(ctx, o.proxyBaseURL+"/api/v1/nodes/"+o.cfg.NodeName, "kubelet", &node); err != nil {
		return fmt.Errorf("read node through hub: %w", err)
	}
	if node.Metadata.Name != o.cfg.NodeName {
		return fmt.Errorf("hub returned an unexpected node")
	}
	return nil
}

func (o *Operations) get(ctx context.Context, target, userAgent string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := o.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // response body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if into == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// workerFileExists follows symlinks the way they resolve inside the worker.
// Kubelet links kubelet-client-current.pem with an absolute path, which os.Root
// refuses to follow, so absolute targets are re-rooted at the worker.
func workerFileExists(root *os.Root, name string) bool {
	for range 8 {
		info, err := root.Lstat(name)
		if err != nil {
			return false
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return info.Mode().IsRegular()
		}
		target, err := root.Readlink(name)
		if err != nil {
			return false
		}
		if filepath.IsAbs(target) {
			name = strings.TrimPrefix(filepath.Clean(target), "/")
		} else {
			name = filepath.Join(filepath.Dir(name), target)
		}
	}
	return false
}

func caPublicKeyHash(root *os.Root) (string, error) {
	data, err := root.ReadFile(workerCA)
	if err != nil {
		return "", fmt.Errorf("read worker API server CA: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "", fmt.Errorf("worker API server CA is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse worker API server CA: %w", err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), nil
}

// ensureLink creates a relative symlink or accepts an identical one. Anything
// else at the path belongs to someone else and fails rather than being replaced.
func ensureLink(root *os.Root, path, target string) error {
	current, err := root.Readlink(path)
	if err == nil {
		if current == target {
			return nil
		}
		return fmt.Errorf("hub path %s already exists with different content", path)
	}
	if _, statErr := root.Lstat(path); statErr == nil {
		return fmt.Errorf("hub path %s already exists and is not a link", path)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("inspect hub path %s: %w", path, statErr)
	}
	if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create hub path directory: %w", err)
	}
	if err := root.Symlink(target, path); err != nil {
		return fmt.Errorf("link hub path %s: %w", path, err)
	}
	return nil
}

func removeDummyInterface(ctx context.Context, log *slog.Logger) error {
	if _, err := net.InterfaceByName(hubDummyIf); err != nil {
		return nil // already absent
	}
	if _, err := utilexec.OutputCmd(ctx, log, "ip", "link", "delete", hubDummyIf); err != nil {
		return fmt.Errorf("remove hub dummy interface: %w", err)
	}
	return nil
}
