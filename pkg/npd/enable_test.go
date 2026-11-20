package npd

import (
	"log/slog"
	"os"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestNPDRequiresOptIn(t *testing.T) {
	t.Parallel()

	tests := map[string]config.NPDConfig{
		"default":      {},
		"version only": {Version: "v1.2.3"},
		"enabled":      {Enabled: true},
	}
	for name, npd := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeContainer, isolation.ModeNone} {
				t.Run(string(mode), func(t *testing.T) {
					t.Parallel()
					cfg := &config.Config{Npd: npd}
					root := isolation.DirRoot{Dir: t.TempDir()}
					log := slog.New(slog.DiscardHandler)
					download := Download(log, cfg, root)
					start := Start(log, cfg, root, &goalstates.NodeStart{}, mode)
					checks := Preflight(cfg)

					if npd.Enabled {
						if task, ok := download.(*downloadTask); !ok || task.version != DefaultVersion {
							t.Fatalf("Download() = %#v, want enabled task with default version %s", download, DefaultVersion)
						}
						if _, ok := start.(*startTask); !ok {
							t.Fatalf("Start() = %T, want *startTask", start)
						}
						if len(checks) != 1 {
							t.Fatalf("Preflight() returned %d checks, want 1", len(checks))
						}
						return
					}

					if len(checks) != 0 {
						t.Fatalf("disabled Preflight() returned %d checks, want 0", len(checks))
					}
					for _, task := range []phases.Task{download, start} {
						if _, ok := task.(disabledTask); !ok {
							t.Fatalf("%s task = %T, want disabledTask", task.Name(), task)
						}
						for range 2 {
							if err := task.Do(t.Context()); err != nil {
								t.Fatalf("%s: %v", task.Name(), err)
							}
						}
					}
					entries, err := os.ReadDir(root.Dir)
					if err != nil {
						t.Fatal(err)
					}
					if len(entries) != 0 {
						t.Fatalf("disabled NPD wrote %d entries, want none", len(entries))
					}
				})
			}
		})
	}
}
