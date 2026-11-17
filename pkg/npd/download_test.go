package npd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

func TestConstructDownloadSource(t *testing.T) {
	t.Parallel()

	got, err := constructDownloadSource(&config.Config{}, "v1.35.1")
	if err != nil {
		t.Fatalf("constructDownloadSource() error = %v", err)
	}
	want := "https://github.com/kubernetes/node-problem-detector/releases/download/v1.35.1/node-problem-detector-v1.35.1-linux_" + runtime.GOARCH + ".tar.gz"
	if got.String() != want {
		t.Fatalf("source = %q, want %q", got.String(), want)
	}
}

func TestDownloadUsesNodeRoot(t *testing.T) {
	t.Parallel()
	fixture, err := filepath.Abs("testdata/version.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"nspawn", "container", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name, configured, wantVersion string
			}{
				{name: "default version", wantVersion: DefaultVersion},
				{name: "configured version", configured: "v1.2.3", wantVersion: "v1.2.3"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					dir := t.TempDir()
					var root isolation.NodeRoot = isolation.DirRoot{Dir: dir}
					binary := filepath.Join(dir, "usr/local/bin/node-problem-detector")
					switch mode {
					case "container":
						root = container.Root{SlotName: "kube1", Dir: dir, HostRoot: t.TempDir()}
					case "none":
						layout := none.Layout{Base: t.TempDir()}
						root = none.Root{HostRoot: dir, Layout: layout, Version: "v1"}
						binary = filepath.Join(layout.VersionDir("v1"), "bin/node-problem-detector")
					}
					cfg := &config.Config{Npd: config.NPDConfig{Version: tc.configured}}
					task, ok := Download(slog.New(slog.DiscardHandler), cfg, root).(*downloadTask)
					if !ok {
						t.Fatal("Download did not return *downloadTask")
					}
					if task.root != root || task.cfg != cfg || task.version != tc.wantVersion {
						t.Fatalf("Download did not retain the node root, config, and version: %+v", task)
					}
					if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
						t.Fatal(err)
					}
					// Keep the executable immutable across parallel process forks.
					if err := os.WriteFile(binary+".version", []byte(tc.wantVersion+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(fixture, binary); err != nil {
						t.Fatal(err)
					}
					// Reuse must succeed without initiating a release download.
					ctx, cancel := context.WithCancel(t.Context())
					cancel()
					for range 2 {
						if err := task.Do(ctx); err != nil {
							t.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func TestPreflight(t *testing.T) {
	t.Parallel()

	checks := Preflight(&config.Config{Npd: config.NPDConfig{Version: "v1.2.3"}})
	if len(checks) != 1 {
		t.Fatalf("Preflight() returned %d checks, want 1", len(checks))
	}
	if got := checks[0].Name(); got != npdArtifactCheckName {
		t.Fatalf("Preflight()[0].Name() = %q, want %q", got, npdArtifactCheckName)
	}
}

func TestOfflineArtifactsDisableNPD(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Bootstrap: config.BootstrapConfig{
			OfflineArtifacts: config.OfflineArtifactsConfig{Source: "file:///opt/artifacts/{{ .KubernetesVersion }}"},
		},
	}

	checks := Preflight(cfg)
	if len(checks) != 1 {
		t.Fatalf("Preflight() returned %d checks, want 1", len(checks))
	}
	results := checks[0].Check(context.Background())
	if len(results) != 1 {
		t.Fatalf("Preflight()[0].Check() returned %d results, want 1", len(results))
	}
	if results[0].Severity != preflight.SeverityWarning {
		t.Fatalf("Preflight()[0].Check()[0].Severity = %q, want warning", results[0].Severity)
	}

	download := Download(slog.Default(), cfg, isolation.DirRoot{Dir: t.TempDir()})
	if download.Name() != "download-npd" {
		t.Fatalf("Download().Name() = %q", download.Name())
	}
	if err := download.Do(context.Background()); err != nil {
		t.Fatalf("Download().Do() error = %v", err)
	}

	if _, ok := download.(disabledTask); !ok {
		t.Fatalf("Download() = %T, want disabledTask", download)
	}

	start := Start(slog.Default(), cfg, isolation.DirRoot{Dir: t.TempDir()}, &goalstates.NodeStart{})
	if start.Name() != "start-npd" {
		t.Fatalf("Start().Name() = %q", start.Name())
	}
	if err := start.Do(context.Background()); err != nil {
		t.Fatalf("Start().Do() error = %v", err)
	}
	if _, ok := start.(*disableTask); !ok {
		t.Fatalf("Start() = %T, want *disableTask", start)
	}
}
