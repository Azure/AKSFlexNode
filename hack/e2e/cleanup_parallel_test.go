package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupBatchesNetworkDeletes(t *testing.T) {
	t.Parallel()
	script, err := filepath.Abs("lib/cleanup.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runShell(t, []string{"SCRIPT=" + script, "E2E_WORK_DIR=" + root}, `
source "$SCRIPT"
E2E_SKIP_CLEANUP=0
GITHUB_RUN_ID=fixture
AZURE_SUBSCRIPTION_ID=fixture
state_get() { echo "$1"; }
log_section() { :; }
log_info() { :; }
log_success() { :; }
az() {
  case "$1 $2" in
    'resource list')
      for type in networkInterfaces publicIPAddresses networkSecurityGroups disks; do
        if [[ "$*" == *"'$type'"* ]]; then
          for i in {1..5}; do echo "/$type/$i"; done
        fi
      done
      ;;
    'resource delete')
      printf 'start\n' >> "$E2E_WORK_DIR/events"
      sleep 0.02
      printf 'end\n' >> "$E2E_WORK_DIR/events"
      ;;
  esac
}
cleanup
`)
	events, err := os.ReadFile(filepath.Join(root, "events"))
	if err != nil {
		t.Fatal(err)
	}
	active, peak, finished := 0, 0, 0
	for _, event := range strings.Fields(string(events)) {
		if event == "start" {
			active++
			peak = max(peak, active)
		} else {
			active--
			finished++
		}
	}
	if active != 0 || peak < 2 || peak > 4 || finished != 20 {
		t.Fatalf("network delete concurrency: active=%d peak=%d finished=%d", active, peak, finished)
	}
}
