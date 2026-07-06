package npd

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestConstructDownloadSource(t *testing.T) {
	t.Parallel()

	arch := runtime.GOARCH

	t.Run("upstream default", func(t *testing.T) {
		t.Parallel()
		got, err := constructDownloadSource(&config.Config{}, "v1.35.1")
		if err != nil {
			t.Fatalf("constructDownloadSource() error = %v", err)
		}
		want := "https://github.com/kubernetes/node-problem-detector/releases/download/v1.35.1/node-problem-detector-v1.35.1-linux_" + arch + ".tar.gz"
		if got.String() != want {
			t.Fatalf("source = %q, want %q", got.String(), want)
		}
	})

	t.Run("file offline source uses unbounded resolved source prefix", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeMinimalOfflineBundle(t, filepath.Join(root, "v1.35.0"), arch)

		cfg := &config.Config{
			Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
			Bootstrap:  config.BootstrapConfig{OfflineArtifacts: config.OfflineArtifactsConfig{Source: "file://" + root + "/{{ .KubernetesVersion }}"}},
		}
		got, err := constructDownloadSource(cfg, "v1.35.1")
		if err != nil {
			t.Fatalf("constructDownloadSource() error = %v", err)
		}
		want := "file://" + root + "/v1.35.0/npd/v1.35.1/node-problem-detector-v1.35.1-linux_" + arch + ".tar.gz"
		if got.String() != want {
			t.Fatalf("source = %q, want %q", got.String(), want)
		}
	})
}

func TestNPDPreflightOfflineFileSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	bundleRoot := filepath.Join(root, "v1.35.0")
	writeMinimalOfflineBundle(t, bundleRoot, runtime.GOARCH)
	artifactPath := filepath.Join(bundleRoot, "npd", "v1.35.1", "node-problem-detector-v1.35.1-linux_"+runtime.GOARCH+".tar.gz")
	writeTestNPDArchive(t, artifactPath)

	cfg := &config.Config{
		Components: config.ComponentsConfig{Kubernetes: "1.35.0"},
		Bootstrap:  config.BootstrapConfig{OfflineArtifacts: config.OfflineArtifactsConfig{Source: "file://" + root + "/{{ .KubernetesVersion }}"}},
	}
	results := Preflight(cfg)[0].Check(context.Background())
	if len(results) != 1 {
		t.Fatalf("results length = %d, want 1", len(results))
	}
	if results[0].Severity != preflight.SeverityOK {
		t.Fatalf("severity = %s, want ok: %#v", results[0].Severity, results[0])
	}
}

func writeMinimalOfflineBundle(t *testing.T, root, arch string) {
	t.Helper()
	manifest := map[string]any{
		"versions": map[string]string{
			"kubernetes": "v1.35.0",
			"containerd": "2.1.8",
			"runc":       "1.5.0",
			"cni":        "1.5.1",
			"crictl":     "1.35.0",
			"npd":        "v1.35.1",
		},
		"containerImages": []string{},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	writeFile(t, filepath.Join(root, "manifest.json"), data)

	for _, binary := range []string{"kubelet", "kubectl", "kube-proxy"} {
		path := filepath.Join(root, "kubernetes", "v1.35.0", "bin", "linux", arch, binary)
		writeFile(t, path, []byte(binary))
		writeFile(t, path+".sha256", []byte("0"))
	}
	writeFile(t, filepath.Join(root, "containerd", "v2.1.8", "containerd-2.1.8-linux-"+arch+".tar.gz"), []byte("containerd"))
	writeFile(t, filepath.Join(root, "runc", "v1.5.0", "runc."+arch), []byte("runc"))
	writeFile(t, filepath.Join(root, "cni", "v1.5.1", "cni-plugins-linux-"+arch+"-v1.5.1.tgz"), []byte("cni"))
	writeFile(t, filepath.Join(root, "crictl", "v1.35.0", "crictl-v1.35.0-linux-"+arch+".tar.gz"), []byte("crictl"))
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func writeTestNPDArchive(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer file.Close()

	gz := gzip.NewWriter(file)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	entries := map[string]string{
		"bin/node-problem-detector":  "#!/bin/sh\necho v1.35.1\n",
		"config/kernel-monitor.json": "{}\n",
	}
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("write body: %v", err)
		}
	}
}
