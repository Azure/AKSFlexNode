package npd

import (
	"context"
	"log/slog"
	"runtime"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
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

	download := Download(slog.Default(), cfg, t.TempDir())
	if download.Name() != "download-npd" {
		t.Fatalf("Download().Name() = %q", download.Name())
	}
	if err := download.Do(context.Background()); err != nil {
		t.Fatalf("Download().Do() error = %v", err)
	}

	if _, ok := download.(disabledTask); !ok {
		t.Fatalf("Download() = %T, want disabledTask", download)
	}

	start := Start(slog.Default(), cfg, &goalstates.NodeStart{})
	if start.Name() != "start-npd" {
		t.Fatalf("Start().Name() = %q", start.Name())
	}
	if err := start.Do(context.Background()); err != nil {
		t.Fatalf("Start().Do() error = %v", err)
	}
	if _, ok := start.(disabledTask); !ok {
		t.Fatalf("Start() = %T, want disabledTask", start)
	}
}
