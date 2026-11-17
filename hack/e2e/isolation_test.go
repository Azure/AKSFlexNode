package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runShellResult(t *testing.T, env []string, script string, wantSuccess bool) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-ceu", script)
	cmd.Env = append(os.Environ(), env...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil || (err == nil) != wantSuccess {
		t.Fatalf("shell error = %v, want success %t\n%s", err, wantSuccess, output)
	}
	return string(output)
}

func isolationScript(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs("lib/isolation.sh")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIsolationReapplyAndRestart(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"none", "container"} {
		for _, scenario := range []struct {
			name    string
			success bool
		}{
			{name: "restart", success: true},
			{name: "reapply-slot-flip"},
			{name: "reapply-node-uid"},
			{name: "reapply-credentials"},
			{name: "reapply-config"},
			{name: "drift-config"},
			{name: "noop-reboot"},
			{name: "host-reboot"},
			{name: "first-process-unchanged"},
			{name: "second-process-unchanged"},
			{name: "restart-slot-flip"},
			{name: "restart-credentials"},
		} {
			t.Run(mode+"/"+scenario.name, func(t *testing.T) {
				t.Parallel()
				if mode == "container" && scenario.name == "drift-config" {
					t.Skip("changed input is exercised for none")
				}

				runShellResult(t, []string{
					"SCRIPT=" + isolationScript(t),
					"ROOT=" + t.TempDir(),
					"MODE=" + mode,
					"SCENARIO=" + scenario.name,
				}, `
source "$SCRIPT"
if [[ "$MODE" == none ]]; then slot=kube1; other=kube2; else slot=kube2; other=kube1; fi
printf '%s\n' "$MODE|$slot|1.35.0|settings|location" > "$ROOT/snapshot"
echo uid > "$ROOT/uid"
echo cert > "$ROOT/cert"
echo state-hash\|config-hash > "$ROOT/config"
echo boot\|first\|second > "$ROOT/observation"
_isolation_snapshot() { command cat "$ROOT/snapshot"; }
_isolation_credentials() { command cat "$ROOT/cert"; }
_isolation_config_digests() { command cat "$ROOT/config"; }
_isolation_restart_observation() { command cat "$ROOT/observation"; }
_isolation_wait_runtime() { :; }
validate_node_joined() { :; }
smoke_test() { :; }
log_error() { echo "$*" >&2; }
kubectl() {
  if [[ "$1" == get ]]; then command cat "$ROOT/uid"; else command cat >/dev/null; fi
}
remote_exec() {
  if [[ "$2" == 'sudo bash -s' ]]; then
    command cat >/dev/null
    [[ "$SCENARIO" != drift-config ]] || echo state-hash\|changed > "$ROOT/config"
  else
    case "$SCENARIO" in
      reapply-slot-flip) echo "$MODE|$other|1.35.0|settings|location" > "$ROOT/snapshot" ;;
      reapply-node-uid) echo other > "$ROOT/uid" ;;
      reapply-credentials) echo other > "$ROOT/cert" ;;
      reapply-config) echo state-hash\|changed > "$ROOT/config" ;;
    esac
  fi
}
_agent_upgrade_wait_phase() {
  case "$SCENARIO" in
    noop-reboot) ;;
    host-reboot) echo newboot\|newfirst\|newsecond > "$ROOT/observation" ;;
    first-process-unchanged) echo boot\|first\|newsecond > "$ROOT/observation" ;;
    second-process-unchanged) echo boot\|newfirst\|second > "$ROOT/observation" ;;
    *) echo boot\|newfirst\|newsecond > "$ROOT/observation" ;;
  esac
  case "$SCENARIO" in
    restart-slot-flip) echo "$MODE|$other|1.35.0|settings|location" > "$ROOT/snapshot" ;;
    restart-credentials) echo other > "$ROOT/cert" ;;
  esac
}
_isolation_reapply_and_restart vm node "$MODE" post-repave
`, scenario.success)
			})
		}
	}
}

func TestIsolationChangedInputUsesTemporaryConfig(t *testing.T) {
	t.Parallel()

	for _, result := range []string{"accepted", "rejected"} {
		t.Run(result, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			config := `{"node":{"maxPods":58},"azure":{"bootstrapToken":{"token":"fixture-secret"}}}`
			if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			agent := `#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == start && "$2" == --config ]]
[[ "$(stat -c '%a' "$3")" == 600 ]]
jq -e '.node.maxPods == 59 and .azure.bootstrapToken.token == "fixture-secret"' "$3" >/dev/null
touch "$ROOT/validated"
[[ "$RESULT" == accepted ]]
`
			if err := os.WriteFile(filepath.Join(root, "agent"), []byte(agent), 0o755); err != nil {
				t.Fatal(err)
			}

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"RESULT=" + result,
			}, `
source "$SCRIPT"
_isolation_snapshot() { echo 'none|kube1|1.35.0|settings|location'; }
_isolation_credentials() { echo cert; }
_isolation_config_digests() { echo state\|config; }
_isolation_wait_runtime() { :; }
validate_node_joined() { :; }
smoke_test() { :; }
log_error() { echo "$*" >&2; }
kubectl() { if [[ "$1" == get ]]; then echo uid; else command cat >/dev/null; fi; }
_isolation_restart_observation() { if [[ -e "$ROOT/restarted" ]]; then echo boot\|new\|new; else echo boot\|old\|old; fi; }
_agent_upgrade_wait_phase() { touch "$ROOT/restarted"; }
remote_exec() {
  if [[ "$2" == 'sudo bash -s' ]]; then
    local script
    script="$(</dev/stdin)"
    script="${script//\/etc\/aks-flex-node\//$ROOT\/}"
    script="${script//\/opt\/unbounded\/agent\/bin\/aks-flex-node/$ROOT\/agent}"
    bash -s <<<"$script"
  fi
}
_isolation_reapply_and_restart vm node none post-repave
`, true)
			if !strings.Contains(output, "Changed bootstrap input was "+result) || strings.Contains(output, "fixture-secret") {
				t.Fatalf("unexpected output: %s", output)
			}
			if _, err := os.Stat(filepath.Join(root, "validated")); err != nil {
				t.Fatalf("agent did not receive the expected temporary config: %v", err)
			}
			matches, err := filepath.Glob(filepath.Join(root, "e2e-reapply.*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("temporary config remains: %v, %v", matches, err)
			}
			original, err := os.ReadFile(filepath.Join(root, "config.json"))
			if err != nil || string(original) != config {
				t.Fatalf("installed config changed: %v", err)
			}
		})
	}
}

func TestIsolationResetArtifacts(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		mode string
		path string
	}{
		{mode: "none", path: "etc/unbounded/agent/none-upgrade-journal.json"},
		{mode: "none", path: "etc/unbounded/agent/none-upgrade-config"},
		{mode: "container", path: "etc/unbounded/agent/container-upgrade-config"},
	} {
		for _, kind := range []string{"file", "directory", "dangling-link"} {
			t.Run(tc.mode+"/"+filepath.Base(tc.path)+"/"+kind, func(t *testing.T) {
				t.Parallel()
				root := t.TempDir()
				artifact := filepath.Join(root, tc.path)
				if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "file":
					if err := os.WriteFile(artifact, []byte("fixture"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(artifact, 0o700); err != nil {
						t.Fatal(err)
					}
				case "dangling-link":
					if err := os.Symlink("missing", artifact); err != nil {
						t.Fatal(err)
					}
				}
				binary := filepath.Join(root, "opt/unbounded/agent/bin/aks-flex-node")
				if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(binary, []byte("fixture"), 0o755); err != nil {
					t.Fatal(err)
				}

				env := []string{"SCRIPT=" + isolationScript(t), "ROOT=" + root, "MODE=" + tc.mode}
				script := `
source "$SCRIPT"
remote_exec() {
  local script
  script="$(</dev/stdin)"
  script="${script//\/etc\//$ROOT\/etc\/}"
  script="${script//\/var\//$ROOT\/var\/}"
  script="${script//\/opt\//$ROOT\/opt\/}"
  bash -s -- "$MODE" <<<"$script"
}
_isolation_assert_clean vm "$MODE"
`
				output := runShellResult(t, env, script, false)
				if !strings.Contains(output, "Reset left "+artifact) {
					t.Fatalf("missing leftover diagnostic: %s", output)
				}
				if err := os.Remove(artifact); err != nil {
					t.Fatal(err)
				}
				runShellResult(t, env, script, true)
			})
		}
	}
}

func TestIsolationConfigCoherence(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		stateValue string
		config     string
		success    bool
	}{
		{name: "matching", stateValue: "58", config: "maxPods: 58", success: true},
		{name: "different", stateValue: "58", config: "maxPods: 59"},
		{name: "missing-state", stateValue: "null", config: "maxPods: 58"},
		{name: "missing-rendered", stateValue: "58", config: "kind: KubeletConfiguration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			state := `{"nodeConfig":{"Kubelet":{"Configuration":{"maxPods":` + tc.stateValue + `}}}}`
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(tc.config+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runShellResult(t, []string{"SCRIPT=" + isolationScript(t), "ROOT=" + root}, `
source "$SCRIPT"
remote_exec() {
  local script
  script="$(</dev/stdin)"
  script="${script//\/etc\/aks-flex-node\/daemon-state.json/$ROOT\/state.json}"
  script="${script//\/var\/lib\/kubelet\/config.yaml/$ROOT\/config.yaml}"
  bash -s <<<"$script"
}
_isolation_config_digests vm
`, tc.success)
		})
	}
}

func TestIsolationStateHistoryUnchanged(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		mutation string
		success  bool
	}{
		{name: "canonical formatting", mutation: ".", success: true},
		{name: "applied version", mutation: `.appliedKubernetesVersion = "0.0.0"`},
		{name: "previous settings", mutation: `del(.previousSettingsVersion)`},
		{name: "previous version", mutation: `del(.previousKubernetesVersion)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			state := `{
  "previousKubernetesVersion": "1.34.0",
  "previousSettingsVersion": "old-settings",
  "appliedKubernetesVersion": "1.35.0",
  "appliedSettingsVersion": "new-settings",
  "activeMachine": "kube2",
  "isolation": "container",
  "nodeConfig": {
    "Kubelet": {
      "Configuration": {
        "maxPods": 58
      },
      "Auth": {
        "BootstrapToken": "fixture-secret"
      }
    }
  }
}`
			if err := os.WriteFile(filepath.Join(root, "state.json"), []byte(state), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("maxPods: 58\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"MUTATION=" + tc.mutation,
			}, `
source "$SCRIPT"
remote_exec() {
  local script
  script="$(</dev/stdin)"
  script="${script//\/etc\/aks-flex-node\/daemon-state.json/$ROOT\/state.json}"
  script="${script//\/var\/lib\/kubelet\/config.yaml/$ROOT\/config.yaml}"
  bash -s <<<"$script"
}
_isolation_snapshot() { echo 'container|kube2|1.35.0|new-settings|location'; }
_isolation_credentials() { echo cert; }
kubectl() { echo uid; }
log_error() { echo "$*" >&2; }
baseline="$(_isolation_config_digests vm)"
_isolation_assert_unchanged vm node 'container|kube2|1.35.0|new-settings|location' uid cert "$baseline" baseline
jq -S "$MUTATION" "$ROOT/state.json" > "$ROOT/next.json"
mv "$ROOT/next.json" "$ROOT/state.json"
_isolation_assert_unchanged vm node 'container|kube2|1.35.0|new-settings|location' uid cert "$baseline" reapply
`, tc.success)
			if strings.Contains(output, "fixture-secret") {
				t.Fatalf("state contents leaked: %s", output)
			}
			if !tc.success && !strings.Contains(output, "reapply changed worker state") {
				t.Fatalf("missing state-change diagnostic: %s", output)
			}
		})
	}
}

func TestIsolationRestartObservation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		mode       string
		invocation string
		state      string
		success    bool
	}{
		{name: "none", mode: "none", invocation: "1234567890abcdef1234567890abcdef", success: true},
		{name: "container", mode: "container", invocation: "1234567890abcdef1234567890abcdef", state: `{"status":"running","pid":123}`, success: true},
		{name: "missing invocation", mode: "none"},
		{name: "zero invocation", mode: "container", invocation: "00000000000000000000000000000000", state: `{"status":"running","pid":123}`},
		{name: "stopped container", mode: "container", invocation: "1234567890abcdef1234567890abcdef", state: `{"status":"stopped","pid":123}`},
		{name: "missing pid", mode: "container", invocation: "1234567890abcdef1234567890abcdef", state: `{"status":"running"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "boot"), []byte("host-boot\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			runc := `#!/usr/bin/env bash
set -euo pipefail
[[ $# -eq 2 && "$1" == state && "$2" == unbounded-kube2 ]]
printf '%s\n' "$RUNC_STATE"
`
			if err := os.WriteFile(filepath.Join(root, "runc"), []byte(runc), 0o755); err != nil {
				t.Fatal(err)
			}

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"MODE=" + tc.mode,
				"INVOCATION=" + tc.invocation,
				"RUNC_STATE=" + tc.state,
			}, `
source "$SCRIPT"
systemctl() {
  [[ $# -eq 4 && "$1" == show && "$3" == --property=InvocationID && "$4" == --value ]] || return 1
  case "$MODE:$2" in
    none:unbounded-kubelet.service|container:unbounded-node-kube2.service) printf '%s\n' "$INVOCATION" ;;
    none:unbounded-containerd.service) echo abcdefabcdefabcdefabcdefabcdefab ;;
    *) return 1 ;;
  esac
}
export -f systemctl
remote_exec() {
  local script
  script="$(</dev/stdin)"
  script="${script//\/proc\/sys\/kernel\/random\/boot_id/$ROOT\/boot}"
  script="${script//\/opt\/unbounded\/runtime\/runc/$ROOT\/runc}"
  bash -s -- "$MODE" kube2 <<<"$script"
}
_isolation_restart_observation vm "$MODE" kube2
`, tc.success)
			if tc.success {
				second := "123"
				if tc.mode == "none" {
					second = "abcdefabcdefabcdefabcdefabcdefab"
				}
				want := "host-boot|" + tc.invocation + "|" + second
				if strings.TrimSpace(output) != want {
					t.Fatalf("observation = %q, want %q", output, want)
				}
			}
		})
	}
}
