package daemon

import (
	"strings"
	"testing"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestSetupHostDeactivatesConflictingServices(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mode           isolation.Mode
		containerized  bool
		wantContainerd bool
		wantKubelet    bool
	}{
		"host-installed agent with nspawn": {
			mode:           isolation.ModeNSpawn,
			wantContainerd: true,
			wantKubelet:    true,
		},
		"containerized agent with nspawn": {
			mode:          isolation.ModeNSpawn,
			containerized: true,
			wantKubelet:   true,
		},
		"host-installed agent with OCI": {
			mode:           isolation.ModeContainer,
			wantContainerd: true,
			wantKubelet:    true,
		},
		"host-installed agent with none": {
			mode: isolation.ModeNone,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{
				Bootstrap: config.BootstrapConfig{Isolation: tc.mode},
			}
			tasks := setupHost(cfg, discardLogger(), tc.containerized).Name()
			for task, want := range map[string]bool{
				"disable-containerd": tc.wantContainerd,
				"disable-kubelet":    tc.wantKubelet,
			} {
				if got := strings.Contains(tasks, task); got != want {
					t.Errorf("setupHost includes %s = %v, want %v (tasks: %s)", task, got, want, tasks)
				}
			}
		})
	}
}
