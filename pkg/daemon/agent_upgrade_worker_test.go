package daemon

import (
	"log/slog"
	"reflect"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func TestAgentUpgradeHelperPathsFollowWorkerRuntime(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		mode    isolation.Mode
		want    map[string]string
		wantErr bool
	}{
		{
			name: "nspawn keeps both slots across repave",
			mode: isolation.ModeNSpawn,
			want: map[string]string{
				"kube1": "/var/lib/machines/kube1/usr/local/bin/aks-flex-node",
				"kube2": "/var/lib/machines/kube2/usr/local/bin/aks-flex-node",
			},
		},
		{
			name: "none has no guest destination",
			mode: isolation.ModeNone,
		},
		{
			name:    "invalid mode",
			mode:    "other",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := agentUpgradeHelperPaths(slog.New(slog.DiscardHandler), tc.mode)
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("helper destinations = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}
