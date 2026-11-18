package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostSoftReboot(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"nspawn", "none", "container"} {
		for _, scenario := range []struct {
			name    string
			success bool
			want    string
		}{
			{
				name:    "recovered",
				success: true,
			},
			{
				name: "mode-mismatch",
				want: "worker isolation does not match",
			},
			{
				name: "same-boot",
				want: "did not produce a new host boot",
			},
			{
				name: "same-agent",
				want: "did not produce a new host boot",
			},
			{
				name: "operation-failed",
				want: "did not reach Complete",
			},
			{
				name: "failed-before-reboot",
				want: "failed before a new host boot",
			},
			{
				name: "wrong-phase",
				want: "expected target boot ID",
			},
			{
				name: "unfinished-target",
				want: "expected target boot ID",
			},
			{
				name: "missing-target",
				want: "expected target boot ID",
			},
			{
				name: "wrong-machine",
				want: "expected target boot ID",
			},
			{
				name: "wrong-boot",
				want: "expected target boot ID",
			},
			{
				name: "wrong-reason",
				want: "expected target boot ID",
			},
			{
				name: "premature-complete",
				want: "completed before the new host boot",
			},
			{
				name: "invalid-completion-time",
				want: "completed before the new host boot",
			},
			{
				name: "slot-loss",
				want: "changed worker state",
			},
			{
				name: "uid-loss",
				want: "changed worker state",
			},
			{
				name: "credential-loss",
				want: "changed worker state",
			},
			{
				name: "state-loss",
				want: "changed worker state",
			},
			{
				name: "config-loss",
				want: "changed worker state",
			},
		} {
			t.Run(mode+"/"+scenario.name, func(t *testing.T) {
				t.Parallel()

				upgradeScript, err := filepath.Abs("lib/agent-upgrade.sh")
				if err != nil {
					t.Fatal(err)
				}
				root := t.TempDir()
				output := runShellResult(t, []string{
					"SCRIPT=" + isolationScript(t),
					"UPGRADE_SCRIPT=" + upgradeScript,
					"ROOT=" + root,
					"MODE=" + mode,
					"SCENARIO=" + scenario.name,
				}, `
source "$SCRIPT"
source "$UPGRADE_SCRIPT"
log_error() { echo "$*" >&2; }
_isolation_snapshot() {
  [[ "$2" == "$MODE" ]]
  if [[ -e "$ROOT/applied" && "$SCENARIO" == slot-loss ]]; then
    slot=kube2
  else
    slot=kube1
  fi
  if [[ "$SCENARIO" == mode-mismatch ]]; then
    echo "other|$slot|1.35.0|settings|location"
    return
  fi
  echo "$MODE|$slot|1.35.0|settings|location"
}
_isolation_credentials() {
  [[ "$2" == "$MODE" && "$3" == kube1 ]]
  if [[ -e "$ROOT/applied" && "$SCENARIO" == credential-loss ]]; then
    echo changed
  else
    echo cert
  fi
}
_isolation_config_digests() {
  [[ "$2" == "$MODE" ]]
  if [[ -e "$ROOT/applied" && "$SCENARIO" == state-loss ]]; then
    echo changed\|config
  elif [[ -e "$ROOT/applied" && "$SCENARIO" == config-loss ]]; then
    echo state\|changed
  else
    echo state\|config
  fi
}
_host_soft_reboot_observation() {
  if [[ ! -e "$ROOT/applied" ]]; then
    echo oldboot\|oldagent\|100
    return
  fi
  if [[ ! -e "$ROOT/disconnected" ]]; then
    touch "$ROOT/disconnected"
    return 255
  fi
  case "$SCENARIO" in
    same-boot|failed-before-reboot) echo oldboot\|oldagent\|100 ;;
    same-agent) echo newboot\|oldagent\|200 ;;
    *) echo newboot\|newagent\|200 ;;
  esac
}
kubectl() {
  case "$1:$2" in
    apply:-f)
      command cat > "$ROOT/request"
      grep -qx '  machineRef: node' "$ROOT/request"
      grep -qx '  operationKind: HostSoftReboot' "$ROOT/request"
      touch "$ROOT/applied"
      ;;
    get:node)
      if [[ -e "$ROOT/applied" && "$SCENARIO" == uid-loss ]]; then
        echo changed
      else
        echo uid
      fi
      ;;
    wait:machineoperation/*)
      [[ -e "$ROOT/disconnected" && "$3" == '--for=jsonpath={.status.phase}=Complete' && "$4" == --timeout=3s ]]
      if [[ "$SCENARIO" == operation-failed ]]; then
        echo fixture-secret
        return 1
      fi
      # The operation may remain InProgress after SSH reconnects.
      echo InProgress > "$ROOT/progress"
      touch "$ROOT/waited"
      ;;
    get:machineoperation)
      if [[ "$*" == *'jsonpath={.status.phase}'* ]]; then
        if [[ "$SCENARIO" == failed-before-reboot ]]; then
          echo Failed
        else
          echo InProgress
        fi
        return
      fi
      [[ -e "$ROOT/waited" ]]
      phase=Complete
      target_phase=Complete
      target=node
      boot=oldboot
      reason=Succeeded
      completed=1970-01-01T00:03:21Z
      case "$SCENARIO" in
        wrong-phase) phase=InProgress ;;
        unfinished-target) target_phase=InProgress ;;
        wrong-machine) target=other ;;
        wrong-boot) boot=other ;;
        wrong-reason) reason=Failed ;;
        premature-complete) completed=1970-01-01T00:03:19Z ;;
        invalid-completion-time) completed=fixture-secret ;;
      esac
      if [[ "$SCENARIO" == missing-target ]]; then
        target=''
      fi
      jq -n --arg phase "$phase" --arg target_phase "$target_phase" --arg target "$target" --arg boot "$boot" --arg reason "$reason" --arg completed "$completed" \
        '{status:{phase:$phase,completedAt:$completed,targets:(if $target == "" then [] else [{machineRef:$target,bootID:$boot,phase:$target_phase}] end),conditions:[{type:"Completed",status:"True",reason:$reason}],message:"fixture-secret"}}'
      ;;
    *) echo "unexpected kubectl call" >&2; return 1 ;;
  esac
}
_isolation_wait_runtime() { [[ -e "$ROOT/waited" && "$2" == "$MODE" ]]; }
validate_node_joined() { [[ -e "$ROOT/waited" ]]; }
smoke_test() { [[ -e "$ROOT/waited" && "$1" == node && "$2" == fixture-host-soft-reboot ]]; echo workload; }
unset SECONDS
SECONDS=0
sleep() { touch "$ROOT/slept"; SECONDS=$((SECONDS + 1)); }
E2E_NODE_JOIN_TIMEOUT=3
E2E_AGENT_UPGRADE_TIMEOUT=3
_host_soft_reboot vm node "$MODE" fixture
`, scenario.success)
				if strings.Contains(output, "fixture-secret") {
					t.Fatalf("status message leaked: %s", output)
				}
				if scenario.want != "" && !strings.Contains(output, scenario.want) {
					t.Fatalf("missing %q: %s", scenario.want, output)
				}
				if scenario.success && !strings.Contains(output, "workload") {
					t.Fatalf("workload did not run: %s", output)
				}
				if scenario.name == "failed-before-reboot" {
					for _, name := range []string{"slept", "disconnected", "waited"} {
						if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
							t.Fatalf("continued waiting after terminal failure: %s: %v", name, err)
						}
					}
				}
			})
		}
	}
}

func TestHostSoftRebootObservation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		invocation string
		boot       string
		btime      string
		success    bool
	}{
		{
			name:       "active",
			invocation: "1234567890abcdef1234567890abcdef",
			boot:       "boot",
			btime:      "200",
			success:    true,
		},
		{
			name:       "missing boot",
			invocation: "1234567890abcdef1234567890abcdef",
			btime:      "200",
		},
		{
			name:  "missing invocation",
			boot:  "boot",
			btime: "200",
		},
		{
			name:       "zero invocation",
			invocation: "00000000000000000000000000000000",
			boot:       "boot",
			btime:      "200",
		},
		{
			name:       "missing boot time",
			invocation: "1234567890abcdef1234567890abcdef",
			boot:       "boot",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			for name, content := range map[string]string{"boot": tc.boot, "stat": "btime " + tc.btime + "\n"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			systemctl := `#!/usr/bin/env bash
case "$1" in
  is-active) [[ "$2" == --quiet && "$3" == aks-flex-node-agent.service ]] ;;
  show) [[ "$2" == aks-flex-node-agent.service ]] && echo "$INVOCATION" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(root, "systemctl"), []byte(systemctl), 0o700); err != nil {
				t.Fatal(err)
			}

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"PATH=" + root + ":" + os.Getenv("PATH"),
				"INVOCATION=" + tc.invocation,
			}, `
source "$SCRIPT"
remote_exec() {
  [[ "$E2E_SSH_OPTS" == *'ConnectTimeout=10'* && "$E2E_SSH_OPTS" == *'ServerAliveCountMax=2'* ]]
  script="$(</dev/stdin)"
  script="${script//\/proc\/sys\/kernel\/random\/boot_id/$ROOT\/boot}"
  script="${script//\/proc\/stat/$ROOT\/stat}"
  bash -s <<<"$script"
}
_host_soft_reboot_observation vm
`, tc.success)
			if tc.success && strings.TrimSpace(output) != "boot|"+tc.invocation+"|200" {
				t.Fatalf("unexpected observation: %s", output)
			}
		})
	}
}

func TestHostSoftRebootNspawnDispatch(t *testing.T) {
	t.Parallel()

	output := runShell(t, []string{"SCRIPT=" + isolationScript(t)}, `
source "$SCRIPT"
_agent_upgrade_ensure_api() { echo api; }
state_get() { echo "$1"; }
_host_soft_reboot() { echo "$*"; }
host_soft_reboot_all
`)
	want := []string{"api"}
	for _, mode := range []string{"msi", "token", "offline", "kubeadm", "arc"} {
		want = append(want, mode+"_vm_ip "+mode+"_vm_name nspawn "+mode)
	}
	if strings.TrimSpace(output) != strings.Join(want, "\n") {
		t.Fatalf("dispatch differs:\n%s", output)
	}
}
