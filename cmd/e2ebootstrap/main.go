// Command e2ebootstrap fetches config from the RP and bootstraps a FlexNode.
// When --dry-run=false, it downloads and installs runc, containerd, kubelet,
// CNI, and starts services inside the container.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/Azure/AKSFlexNode/pkg/rpclient"
)

var log = logrus.New()

func main() {
	rpURL := flag.String("rp", "http://localhost:18081", "RP base URL")
	sub := flag.String("sub", "8ecadfc9-d1a3-4ea4-b844-0d9f87e4d7c8", "Subscription ID")
	rg := flag.String("rg", "flexnode-test-35468", "Resource group")
	cluster := flag.String("cluster", "fntest35468", "Cluster name")
	machine := flag.String("machine", "", "Machine name (default: hostname)")
	dryRun := flag.Bool("dry-run", false, "Print config only, skip install")
	flag.Parse()

	log.SetLevel(logrus.InfoLevel)
	log.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	name := *machine
	if name == "" {
		name, _ = os.Hostname()
	}

	passed, failed := 0, 0
	check := func(step string, err error) bool {
		if err != nil {
			fmt.Printf("  ✗ %s: %v\n", step, err)
			failed++
			return false
		}
		fmt.Printf("  ✓ %s\n", step)
		passed++
		return true
	}

	client := rpclient.NewStandaloneClient(log, *rpURL, *sub, *rg, *cluster, name)

	// --- 1. Register ---
	fmt.Println("\n=== 1. Register machine ===")
	check("PUT machine", client.RegisterMachine(ctx, map[string]string{
		"hostname":  name,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}))

	// --- 2. Get bootstrap data ---
	fmt.Println("\n=== 2. Fetch bootstrap data ===")
	bd, err := client.GetBootstrapData(ctx)
	if !check("POST getBootstrapData", err) {
		os.Exit(1)
	}

	b, _ := json.MarshalIndent(bd, "  ", "  ")
	fmt.Printf("  %s\n", b)

	if *dryRun {
		fmt.Println("\n=== Dry-run — stopping here ===")
		printResults(passed, failed)
		return
	}

	// --- 3. Download and install binaries ---
	fmt.Println("\n=== 3. Install binaries ===")

	if bd.Binaries != nil && bd.Binaries.Runc != nil {
		check("install runc "+bd.Binaries.Runc.Version,
			downloadFile(ctx, bd.Binaries.Runc.URL, "/usr/local/bin/runc", 0755))
	}

	if bd.Binaries != nil && bd.Binaries.Containerd != nil {
		check("install containerd "+bd.Binaries.Containerd.Version,
			installTarGz(ctx, bd.Binaries.Containerd.URL, "/usr/local"))
	}

	if bd.Binaries != nil && bd.Binaries.Kubelet != nil {
		check("install kubelet "+bd.Binaries.Kubelet.Version,
			downloadFile(ctx, bd.Binaries.Kubelet.URL, "/usr/local/bin/kubelet", 0755))
	}

	if bd.CNI != nil && bd.CNI.BinaryURL != "" {
		os.MkdirAll("/opt/cni/bin", 0755)
		check("install CNI "+bd.CNI.Version,
			installTarGz(ctx, bd.CNI.BinaryURL, "/opt/cni/bin"))
	}

	// --- 4. Verify installed binaries ---
	fmt.Println("\n=== 4. Verify binaries ===")
	for _, bin := range []struct{ name, path, versionFlag string }{
		{"runc", "/usr/local/bin/runc", "--version"},
		{"containerd", "/usr/local/bin/containerd", "--version"},
		{"kubelet", "/usr/local/bin/kubelet", "--version"},
	} {
		out, err := exec.CommandContext(ctx, bin.path, bin.versionFlag).CombinedOutput()
		if err != nil {
			check(bin.name+" version", fmt.Errorf("%v: %s", err, string(out)))
		} else {
			version := strings.TrimSpace(strings.Split(string(out), "\n")[0])
			check(bin.name+" version: "+version, nil)
		}
	}

	// --- 5. Write configs ---
	fmt.Println("\n=== 5. Write configs ===")

	// Containerd config
	os.MkdirAll("/etc/containerd", 0755)
	pauseImage := "mcr.microsoft.com/oss/kubernetes/pause:3.9"
	if bd.Images != nil && bd.Images.Pause != "" {
		pauseImage = bd.Images.Pause
	}
	containerdConfig := fmt.Sprintf(`version = 3
[plugins."io.containerd.cri.v1.images"]
  sandbox_image = "%s"
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.runc.options]
  SystemdCgroup = true
`, pauseImage)
	check("write /etc/containerd/config.toml",
		os.WriteFile("/etc/containerd/config.toml", []byte(containerdConfig), 0644))

	// CNI config
	os.MkdirAll("/etc/cni/net.d", 0755)
	cniConfig := fmt.Sprintf(`{
  "cniVersion": "0.3.1",
  "name": "flexnode-bridge",
  "type": "bridge",
  "bridge": "cni0",
  "isGateway": true,
  "ipMasq": true,
  "ipam": {
    "type": "host-local",
    "subnet": "%s",
    "routes": [{"dst": "0.0.0.0/0"}]
  }
}`, bd.PodCIDR)
	check("write /etc/cni/net.d/10-bridge.conf",
		os.WriteFile("/etc/cni/net.d/10-bridge.conf", []byte(cniConfig), 0644))

	// Kubelet bootstrap kubeconfig
	os.MkdirAll("/var/lib/kubelet", 0755)
	os.MkdirAll("/etc/kubernetes/pki", 0755)

	// Write CA cert file
	check("write ca.crt for bootstrap",
		os.WriteFile("/etc/kubernetes/pki/ca.crt", []byte(bd.CACertData), 0644))

	bootstrapKubeconfig := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    certificate-authority: /etc/kubernetes/pki/ca.crt
    server: https://%s:443
  name: default
contexts:
- context:
    cluster: default
    user: kubelet-bootstrap
  name: default
current-context: default
users:
- name: kubelet-bootstrap
  user:
    token: %s
`, bd.ClusterFQDN, bd.BootstrapToken)
	check("write bootstrap-kubeconfig",
		os.WriteFile("/var/lib/kubelet/bootstrap-kubeconfig", []byte(bootstrapKubeconfig), 0644))

	// --- 6. Start services ---
	fmt.Println("\n=== 6. Start services ===")

	// Start containerd
	check("start containerd", startDaemon(ctx, "containerd",
		"/usr/local/bin/containerd", "--config", "/etc/containerd/config.toml"))

	// Wait for containerd socket
	time.Sleep(2 * time.Second)
	_, err = os.Stat("/run/containerd/containerd.sock")
	check("containerd socket ready", err)

	// Start kubelet (won't fully join without real cluster certs, but validates the flow)
	maxPods := 110
	if bd.Node != nil && bd.Node.MaxPods != nil {
		maxPods = *bd.Node.MaxPods
	}
	dnsIP := "10.0.0.10"
	if bd.ClusterDNS != "" {
		dnsIP = bd.ClusterDNS
	}
	// Write kubelet config file (required for k8s 1.34+)
	os.MkdirAll("/var/lib/kubelet", 0755)
	kubeletConfig := fmt.Sprintf(`apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
authentication:
  anonymous:
    enabled: false
  webhook:
    enabled: true
  x509:
    clientCAFile: /etc/kubernetes/pki/ca.crt
authorization:
  mode: Webhook
cgroupDriver: systemd
clusterDNS:
- %s
clusterDomain: cluster.local
containerRuntimeEndpoint: unix:///run/containerd/containerd.sock
failSwapOn: false
maxPods: %d
resolvConf: /run/systemd/resolve/resolv.conf
`, dnsIP, maxPods)
	check("write kubelet-config.yaml",
		os.WriteFile("/var/lib/kubelet/config.yaml", []byte(kubeletConfig), 0644))

	// Start kubelet with config file
	check("start kubelet", startDaemon(ctx, "kubelet",
		"/usr/local/bin/kubelet",
		"--bootstrap-kubeconfig=/var/lib/kubelet/bootstrap-kubeconfig",
		"--kubeconfig=/var/lib/kubelet/kubeconfig",
		"--config=/var/lib/kubelet/config.yaml",
		"--v=2",
	))

	// Give kubelet time to TLS bootstrap
	time.Sleep(10 * time.Second)

	// Check kubelet is running
	out, err := exec.Command("pgrep", "-x", "kubelet").Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		check("kubelet process running", fmt.Errorf("kubelet not found"))
	} else {
		check("kubelet process running (pid "+strings.TrimSpace(string(out))+")", nil)
	}

	// --- 7. Report status back to RP ---
	fmt.Println("\n=== 7. Report status ===")
	runcVer := ""
	if o, err := exec.Command("/usr/local/bin/runc", "--version").Output(); err == nil {
		runcVer = strings.TrimSpace(strings.Split(string(o), "\n")[0])
	}
	check("PUT status to RP", client.ReportStatus(ctx, map[string]string{
		"agentVersion":      "e2e-test",
		"kubeletRunning":    fmt.Sprintf("%v", err == nil),
		"containerdVersion": bd.Binaries.Containerd.Version,
		"runcVersion":       runcVer,
		"bootstrapComplete": "true",
	}))

	printResults(passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func printResults(passed, failed int) {
	fmt.Printf("\n══════════════════════════════════\n")
	fmt.Printf("  PASSED: %d  FAILED: %d\n", passed, failed)
	fmt.Printf("══════════════════════════════════\n")
}

// downloadFile downloads a URL to a local path.
func downloadFile(ctx context.Context, url, dest string, mode os.FileMode) error {
	os.MkdirAll(filepath.Dir(dest), 0755)
	log.Infof("Downloading %s → %s", url, dest)
	cmd := exec.CommandContext(ctx, "curl", "-fsSL", "-o", dest, url)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("curl %s: %w", url, err)
	}
	return os.Chmod(dest, mode)
}

// installTarGz downloads and extracts a tar.gz to a directory.
func installTarGz(ctx context.Context, url, destDir string) error {
	os.MkdirAll(destDir, 0755)
	log.Infof("Downloading and extracting %s → %s", url, destDir)
	cmd := exec.CommandContext(ctx, "bash", "-c",
		fmt.Sprintf("curl -fsSL '%s' | tar -xz -C '%s'", url, destDir))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// startDaemon starts a process in the background.
func startDaemon(ctx context.Context, name string, args ...string) error {
	log.Infof("Starting %s: %s", name, strings.Join(args, " "))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.NewFile(0, os.DevNull)
	cmd.Stderr = os.NewFile(0, os.DevNull)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	go cmd.Wait() //nolint:errcheck
	log.Infof("%s started (pid %d)", name, cmd.Process.Pid)
	return nil
}
