package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rebootEnv(t *testing.T, root string) []string {
	t.Helper()
	common, err := filepath.Abs("lib/common.sh")
	if err != nil {
		t.Fatal(err)
	}
	return []string{
		"SCRIPT=" + isolationScript(t),
		"COMMON=" + common,
		"ROOT=" + root,
		"E2E_WORK_DIR=" + root,
		"E2E_SSH_USER=fixture",
		"E2E_SSH_OPTS=-o ConnectTimeout=10",
		"E2E_SSH_IDENTITY_FILE=",
		"PATH=" + root + string(os.PathListSeparator) + os.Getenv("PATH"),
	}
}

func TestIsolationHostRebootBoundsSSH(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"initial-probe", "request", "reconnect-probe"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			ssh := `#!/usr/bin/env bash
set -euo pipefail
case "${*: -1}" in
  'cat /proc/sys/kernel/random/boot_id')
    if [[ ! -e "$ROOT/before" ]]; then
      touch "$ROOT/before"
      if [[ "$PHASE" == initial-probe ]]; then exec sleep 30; fi
      echo old-boot
    else
      exec sleep 30
    fi
    ;;
  'sudo systemctl reboot')
    touch "$ROOT/requested"
    if [[ "$PHASE" == request ]]; then exec sleep 30; fi
    exit 255
    ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(ssh), 0o700); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			output := runShellResult(t, append(rebootEnv(t, root), "PHASE="+phase), `
source "$COMMON"
source "$SCRIPT"
E2E_NODE_JOIN_TIMEOUT=1
_isolation_reboot_diagnostics() { echo reboot-diagnostics; }
_isolation_wait_runtime() { echo unexpected-runtime-validation; }
validate_node_joined() { echo unexpected-node-validation; }
smoke_test() { echo unexpected-workload-validation; }
_isolation_host_reboot vm node none
`, false)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("blocked SSH exceeded reboot budget and kill grace: %v", elapsed)
			}
			if !strings.Contains(output, "reboot-diagnostics") || strings.Contains(output, "unexpected-") {
				t.Fatalf("missing diagnostics or validated an unobserved reboot: %s", output)
			}
			if phase == "initial-probe" {
				if _, err := os.Stat(filepath.Join(root, "requested")); !os.IsNotExist(err) {
					t.Fatalf("reboot requested without an initial boot ID: %v", err)
				}
			}
		})
	}
}

func TestIsolationHostRebootRequiresChangedBootID(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"none", "container"} {
		for _, scenario := range []string{"reconnected", "disconnected-request", "unchanged-boot"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				ssh := `#!/usr/bin/env bash
set -euo pipefail
case "${*: -1}" in
  'cat /proc/sys/kernel/random/boot_id')
    if [[ ! -e "$ROOT/requested" || "$SCENARIO" == unchanged-boot ]]; then
      echo old-boot
    elif [[ ! -e "$ROOT/disconnected" ]]; then
      touch "$ROOT/disconnected"
      exit 255
    else
      echo new-boot
    fi
    ;;
  'sudo systemctl reboot')
    touch "$ROOT/requested"
    if [[ "$SCENARIO" == disconnected-request ]]; then exit 255; fi
    ;;
  *) exit 2 ;;
esac
`
				if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(ssh), 0o700); err != nil {
					t.Fatal(err)
				}
				env := append(rebootEnv(t, root), "MODE="+mode, "SCENARIO="+scenario)
				output := runShellResult(t, env, `
source "$COMMON"
source "$SCRIPT"
E2E_NODE_JOIN_TIMEOUT=2
sleep() { :; }
_isolation_reboot_diagnostics() { echo reboot-diagnostics; }
_isolation_wait_runtime() { echo "runtime:$2"; }
validate_node_joined() { echo "node:$1"; }
smoke_test() { echo "workload:$2"; }
_isolation_host_reboot vm node "$MODE"
`, scenario != "unchanged-boot")
				if scenario == "unchanged-boot" {
					if !strings.Contains(output, "reboot-diagnostics") || strings.Contains(output, "runtime:") {
						t.Fatalf("unchanged boot must fail with diagnostics: %s", output)
					}
					return
				}
				for _, want := range []string{"old-boot -> new-boot", "runtime:" + mode, "node:node", "workload:" + mode + "-host-reboot"} {
					if !strings.Contains(output, want) {
						t.Fatalf("missing %q: %s", want, output)
					}
				}
				if strings.Contains(output, "reboot-diagnostics") {
					t.Fatalf("successful reboot should not collect failure diagnostics: %s", output)
				}
			})
		}
	}
}

func TestIsolationRebootDiagnostics(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"reachable", "unreachable"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "boot"), []byte("diagnostic-boot\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			output := runShell(t, append(rebootEnv(t, root), "SCENARIO="+scenario), `
source "$COMMON"
source "$SCRIPT"
systemctl() { printf 'systemctl %s\n' "$*"; }
journalctl() {
  if [[ "$1" != --list-boots && "$*" != *'_PID=1'* ]]; then
    echo fixture-secret
    return 1
  fi
  printf 'journalctl %s\n' "$*"
}
export -f systemctl journalctl
_isolation_reboot_exec() {
  (( $1 - SECONDS > 0 && $1 - SECONDS <= 20 ))
  [[ "$2" == vm && "$3" == 'sudo bash -s' ]]
  if [[ "$SCENARIO" == unreachable ]]; then return 124; fi
  local script
  script="$(</dev/stdin)"
  script="${script//\/proc\/sys\/kernel\/random\/boot_id/$ROOT\/boot}"
  bash -s <<<"$script"
}
_isolation_reboot_diagnostics vm
`)
			if strings.Contains(output, "fixture-secret") {
				t.Fatalf("diagnostics included unrestricted journal entries: %s", output)
			}
			if scenario == "unreachable" {
				if !strings.Contains(output, "diagnostics unavailable (SSH exit 124)") {
					t.Fatalf("missing bounded-SSH failure diagnostic: %s", output)
				}
				return
			}
			for _, want := range []string{
				"diagnostic-boot", "systemctl list-jobs", "systemd-reboot.service",
				"journalctl --list-boots", "journalctl -b 0 _PID=1", "journalctl -b -1 _PID=1",
			} {
				if !strings.Contains(output, want) {
					t.Fatalf("missing %q: %s", want, output)
				}
			}
		})
	}
}
