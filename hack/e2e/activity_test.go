package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActivityDiagnosticsAreScopedAndBestEffort(t *testing.T) {
	t.Parallel()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("E2E scripts require bash: %v", err)
	}
	source, err := filepath.Abs("lib/cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	const clusterID = "/subscriptions/test-sub/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/target"
	for _, tc := range []struct {
		name, subscription, azureStatus, cluster string
	}{
		{name: "metadata", subscription: "test-sub", azureStatus: "0", cluster: clusterID},
		{name: "permission denied is diagnostic only", subscription: "test-sub", azureStatus: "1", cluster: clusterID},
		{name: "no state", azureStatus: "0"},
		{name: "no cluster", subscription: "test-sub", azureStatus: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bash, "-c", `
set -euo pipefail
source "$1"
E2E_LOG_DIR="$2"
input_subscription="$3"
status="$4"
input_cluster="$5"
trace="$2/call"
state_get() {
  case "$1" in subscription_id) printf '%s' "$input_subscription" ;; resource_group) echo test-rg ;; cluster_id) printf '%s' "$input_cluster" ;; esac
}
timeout() { [[ "$1" == 60 ]]; shift; "$@"; }
az() {
  printf '%s\n' "$@" > "$trace"
  if [[ "$status" != 0 ]]; then echo 'Activity Log access denied' >&2; return 1; fi
  printf '[{"caller":"target","resourceId":"%s"},{"caller":"child","resourceId":"%s/agentPools/test-pool"},{"caller":"unrelated","resourceId":"%s-sibling"}]' "$input_cluster" "$input_cluster" "$input_cluster"
}
_collect_aks_activity
`, "test", source, work, tc.subscription, tc.azureStatus, tc.cluster)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("activity diagnostics failed the caller: %v\n%s", err, out)
			}
			call, err := os.ReadFile(filepath.Join(work, "call"))
			if tc.subscription == "" || tc.cluster == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("Azure queried without explicit test state: %s, %v", call, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"monitor\nactivity-log\nlist", "--subscription\ntest-sub", "--resource-group\ntest-rg", "--offset\n4h", "--max-events\n1000", "caller:caller", "operationId:operationId"} {
				if !strings.Contains(string(call), want) {
					t.Errorf("missing diagnostic scope/field %q: %s", want, call)
				}
			}
			if strings.Contains(string(call), "properties") {
				t.Fatal("diagnostics request includes potentially sensitive operation properties")
			}
			if tc.azureStatus == "0" {
				data, err := os.ReadFile(filepath.Join(work, "azure-activity.json"))
				if err != nil {
					t.Fatal(err)
				}
				var events []struct{ Caller string }
				if err := json.Unmarshal(data, &events); err != nil {
					t.Fatal(err)
				}
				if len(events) != 2 || events[0].Caller != "target" || events[1].Caller != "child" {
					t.Fatalf("diagnostics persisted another cluster's caller metadata: %s", data)
				}
			}
		})
	}
}
