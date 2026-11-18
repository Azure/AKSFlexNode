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

// runtimeFixture supplies the commands invoked by the real remote scripts.
func runtimeFixture(t *testing.T, state string) string {
	t.Helper()

	root := t.TempDir()
	files := map[string]string{
		"state.json": state,
		"systemctl": `#!/usr/bin/env bash
case "$1" in
  is-active)
    case "$3" in
      unbounded-node-*.service) check=supervisor ;;
      unbounded-kubelet.service) check=host-kubelet ;;
      unbounded-containerd.service) check=host-containerd ;;
      *) exit 2 ;;
    esac ;;
  status) printf 'status %s\n   CGroup: /system.slice\n  workload fixture-secret\n' "$2"; exit 4 ;;
  show) echo 'ActiveState=active'; exit 0 ;;
  --failed) echo 'no failed units'; exit 0 ;;
  *) exit 2 ;;
esac
if [[ "$FAIL" == "$check" ]]; then echo "$check error" >&2; exit 7; fi
`,
		"journalctl": `#!/usr/bin/env bash
if [[ "${BIG_JOURNAL:-}" == 1 ]]; then
  head -c 16384 /dev/zero | tr '\0' X
  exit 6
fi
echo "journal $*"
`,
		"runc": `#!/usr/bin/env bash
if [[ "$1" == state ]]; then
  if [[ "$FAIL" == runc-state ]]; then echo 'runc state error' >&2; exit 9; fi
  if [[ "$FAIL" == runc-running ]]; then status=stopped; else status=running; fi
  printf '{"id":"%s","status":"%s","pid":123,"annotations":{"credential":"fixture-secret"}}\n' "$2" "$status"
  exit 0
fi
[[ "$1" == exec ]] || exit 2
if [[ "${DRAIN_STDIN:-}" == 1 ]]; then wc -c >> "$ROOT/reads"; fi
case "$3" in
  systemctl)
    if [[ "$4" == status ]]; then printf 'guest status %s\n CGroup: /guest\n fixture-secret\n' "$5"; exit 4; fi
    case "$6" in
      kubelet.service) check=guest-kubelet ;;
      containerd.service) check=guest-containerd ;;
      *) exit 2 ;;
    esac ;;
  /usr/local/bin/kubelet) check=kubelet-version ;;
  *) exit 2 ;;
esac
if [[ "$FAIL" == "$check" ]]; then
  if [[ "$check" == kubelet-version ]]; then printf 'bad version\r\n'; fi
  echo "$check error" >&2
  exit 8
fi
if [[ "$check" == kubelet-version ]]; then printf '%s' "$VERSION_OUTPUT"; fi
`,
		"kubelet": `#!/usr/bin/env bash
if [[ "$FAIL" == kubelet-version ]]; then printf 'bad version\r\n'; echo 'version error' >&2; exit 8; fi
printf '%s' "$VERSION_OUTPUT"
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const runtimeRemoteStub = `
remote_exec() {
  local script
  script="$(</dev/stdin)"
  script="${script//\/etc\/aks-flex-node\/daemon-state.json/$ROOT\/state.json}"
  script="${script//\/opt\/unbounded\/runtime\/runc/$ROOT\/runc}"
  script="${script//\/opt\/unbounded\/node\/current\/bin\/kubelet/$ROOT\/kubelet}"
  printf '%s\n' "$script" | bash -s -- "$EXPECTED"
}
`

func TestIsolationSnapshotExecDoesNotConsumeScript(t *testing.T) {
	t.Parallel()

	root := runtimeFixture(t, `{"isolation":"container","activeMachine":"kube1","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`)
	output := runShellResult(t, []string{
		"SCRIPT=" + isolationScript(t),
		"ROOT=" + root,
		"PATH=" + root + ":" + os.Getenv("PATH"),
		"EXPECTED=container",
		"FAIL=",
		"DRAIN_STDIN=1",
		"VERSION_OUTPUT=Kubernetes v1.35.0\n",
	}, `source "$SCRIPT"`+runtimeRemoteStub+`_isolation_snapshot vm`, true)
	want := "container|kube1|1.35.0|settings|/var/lib/unbounded/containers/kube1\n"
	if output != want {
		t.Fatalf("snapshot = %q, want %q", output, want)
	}
	reads, err := os.ReadFile(filepath.Join(root, "reads"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(string(reads)), ","); got != "0,0,0" {
		t.Fatalf("exec stdin byte counts = %q", got)
	}
}

func TestIsolationSnapshotChecks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		mode string
		fail string
		want string
	}{
		{name: "supervisor", mode: "container", fail: "supervisor", want: "snapshot check supervisor failed (exit 7)"},
		{name: "runc state", mode: "container", fail: "runc-state", want: "snapshot check runc-state failed (exit 9)"},
		{name: "runc stopped", mode: "container", fail: "runc-running", want: "snapshot check runc-running failed (exit 1)"},
		{name: "guest kubelet", mode: "container", fail: "guest-kubelet", want: "snapshot check guest-kubelet failed (exit 8)"},
		{name: "guest containerd", mode: "container", fail: "guest-containerd", want: "snapshot check guest-containerd failed (exit 8)"},
		{name: "guest version", mode: "container", fail: "kubelet-version", want: "snapshot check kubelet-version failed (exit 8)"},
		{name: "host kubelet", mode: "none", fail: "host-kubelet", want: "snapshot check host-kubelet failed (exit 7)"},
		{name: "host containerd", mode: "none", fail: "host-containerd", want: "snapshot check host-containerd failed (exit 7)"},
		{name: "host version", mode: "none", fail: "kubelet-version", want: "snapshot check kubelet-version failed (exit 8)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := `{"isolation":"` + tc.mode + `","activeMachine":"kube2","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`
			root := runtimeFixture(t, state)
			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"PATH=" + root + ":" + os.Getenv("PATH"),
				"EXPECTED=" + tc.mode,
				"FAIL=" + tc.fail,
				"VERSION_OUTPUT=Kubernetes v1.35.0",
			}, `source "$SCRIPT"`+runtimeRemoteStub+`_isolation_snapshot vm`, false)
			if !strings.Contains(output, tc.want) || strings.Contains(output, "fixture-secret") {
				t.Fatalf("unexpected snapshot diagnostic: %s", output)
			}
			if tc.fail == "runc-running" && !strings.Contains(output, "check stdout (first 4096 bytes): false") {
				t.Fatalf("missing failed check output: %s", output)
			}
			if tc.fail == "kubelet-version" && !strings.Contains(output, "check stdout (first 4096 bytes): $'bad version\\r'") {
				t.Fatalf("missing version output: %s", output)
			}
		})
	}
}

func TestIsolationSnapshotMetadata(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state string
		want  string
	}{
		{name: "missing isolation", state: `{"activeMachine":"kube1","appliedSettingsVersion":"settings"}`, want: "snapshot check isolation failed"},
		{name: "unknown isolation", state: `{"isolation":"other","activeMachine":"kube1","appliedSettingsVersion":"settings"}`, want: "snapshot check isolation: invalid mode"},
		{name: "missing slot", state: `{"isolation":"container","appliedSettingsVersion":"settings"}`, want: "snapshot check activeMachine failed"},
		{name: "invalid slot", state: `{"isolation":"container","activeMachine":"other","appliedSettingsVersion":"settings"}`, want: "snapshot check activeMachine: invalid slot"},
		{name: "missing settings", state: `{"isolation":"container","activeMachine":"kube1"}`, want: "snapshot check appliedSettingsVersion failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := runtimeFixture(t, tc.state)
			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"PATH=" + root + ":" + os.Getenv("PATH"),
				"EXPECTED=container",
				"FAIL=",
				"VERSION_OUTPUT=Kubernetes v1.35.0\n",
			}, `source "$SCRIPT"`+runtimeRemoteStub+`_isolation_snapshot vm`, false)
			if !strings.Contains(output, tc.want) {
				t.Fatalf("missing %q: %s", tc.want, output)
			}
		})
	}
}

func TestIsolationRuntimeDiagnostics(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		state      string
		expected   string
		fail       string
		version    string
		bigJournal string
		success    bool
		want       []string
		unwanted   []string
	}{
		{
			name:     "container probe failure",
			state:    `{"isolation":"container","activeMachine":"kube2","appliedSettingsVersion":"line\r\nnext","appliedKubernetesVersion":"1.35.0","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			fail:     "runc-state",
			version:  "Kubernetes v1.35.0\n",
			want: []string{
				"last observation: probe-failed", "Snapshot exit=9", "runc state error", "snapshot check runc-state failed (exit 9)",
				`"appliedSettingsVersion":"line\r\nnext"`, "unbounded-node-kube2.service properties", "unbounded-node-kube2.service journal",
				"unbounded-kube2 state", "unbounded-kube2 kubelet.service status", "unbounded-kube2 containerd.service status", "raw version", "exit=4", "_PID=1", "agent journal", "host failed units",
			},
			unwanted: []string{"unbounded-node-kube1.service", "unbounded-kubelet.service"},
		},
		{
			name:     "mismatched multiline version",
			state:    `{"isolation":"container","activeMachine":"kube1","appliedSettingsVersion":"settings","appliedKubernetesVersion":"1.35.0","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			version:  "Kubernetes v1.35.0\r\nextra\n",
			want: []string{
				"last observation: mismatch", "Snapshot mismatch: expected mode=container version=1.35.0",
				`observed mode=container version=$'1.35.0\r'`,
				`raw version (first 256 bytes): $'Kubernetes v1.35.0\r\nextra\n'`,
				"unbounded-node-kube1.service journal", "unbounded-kube1 containerd.service status",
			},
			unwanted: []string{"unbounded-node-kube2.service"},
		},
		{
			name:     "matching first line of multiline version",
			state:    `{"isolation":"container","activeMachine":"kube1","appliedSettingsVersion":"settings","appliedKubernetesVersion":"1.35.0","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			version:  "Kubernetes v1.35.0\nunexpected\n",
			want: []string{
				"last observation: mismatch", "Snapshot mismatch: expected mode=container version=1.35.0",
				"observed mode=container version=1.35.0",
				`raw version (first 256 bytes): $'Kubernetes v1.35.0\nunexpected\n'`,
				"unbounded-node-kube1.service journal", "unbounded-kube1 containerd.service status",
			},
			unwanted: []string{"unbounded-node-kube2.service"},
		},
		{
			name:     "observed mode selects host",
			state:    `{"isolation":"none","activeMachine":"kube1","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			fail:     "host-kubelet",
			version:  "Kubernetes v1.35.0\n",
			want: []string{
				"snapshot check host-kubelet failed (exit 7)", "unbounded-kubelet.service status", "unbounded-containerd.service journal", "raw version",
			},
			unwanted: []string{"unbounded-node-kube1.service"},
		},
		{
			name:       "bounded output preserves exit and later checks",
			state:      `{"isolation":"container","activeMachine":"kube1","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`,
			expected:   "container",
			fail:       "supervisor",
			version:    "Kubernetes v1.35.0\n",
			bigJournal: "1",
			want: []string{
				"snapshot check supervisor failed (exit 7)", "exit=6", "unbounded-kube1 containerd.service status", "raw version",
			},
		},
		{
			name:     "unknown slot checks both supervisors",
			state:    `{"isolation":"container","activeMachine":"invalid","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			version:  "Kubernetes v1.35.0\n",
			want: []string{
				"snapshot check activeMachine: invalid slot", "unbounded-node-kube1.service status", "unbounded-node-kube2.service status",
			},
		},
		{
			name:     "unknown mode uses expected mode",
			state:    `{"activeMachine":"kube2","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			version:  "Kubernetes v1.35.0\n",
			want: []string{
				"snapshot check isolation failed", "unbounded-node-kube2.service properties", "unbounded-kube2 containerd.service status",
			},
			unwanted: []string{"unbounded-node-kube1.service", "unbounded-kubelet.service"},
		},
		{
			name:     "success is quiet",
			state:    `{"isolation":"container","activeMachine":"kube2","appliedSettingsVersion":"settings","nodeConfig":{"token":"fixture-secret"}}`,
			expected: "container",
			version:  "Kubernetes v1.35.0\n",
			success:  true,
			want:     []string{"Worker container/kube2 runs Kubernetes 1.35.0"},
			unwanted: []string{"last observation", "properties", "journal", "raw version"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := runtimeFixture(t, tc.state)
			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + root,
				"PATH=" + root + ":" + os.Getenv("PATH"),
				"EXPECTED=" + tc.expected,
				"FAIL=" + tc.fail,
				"DRAIN_STDIN=1",
				"BIG_JOURNAL=" + tc.bigJournal,
				"VERSION_OUTPUT=" + tc.version,
			}, `
source "$SCRIPT"
`+runtimeRemoteStub+`
log_error() { echo "$*" >&2; }
log_info() { echo "$*"; }
# Poll attempts must not depend on crossing a wall-clock second during setup.
unset SECONDS
SECONDS=0
sleep() { SECONDS=100; }
E2E_NODE_JOIN_TIMEOUT=1
E2E_KUBERNETES_VERSION=v1.35.0
_isolation_wait_runtime vm "$EXPECTED"
`, tc.success)
			if tc.bigJournal == "1" && len(output) > 21000 {
				t.Fatalf("diagnostic output was not bounded: %d bytes", len(output))
			}
			for _, want := range tc.want {
				if !strings.Contains(output, want) {
					t.Errorf("missing %q in output:\n%s", want, output)
				}
			}
			for _, unwanted := range append(tc.unwanted, "fixture-secret", "nodeConfig", "annotations") {
				if strings.Contains(output, unwanted) {
					t.Errorf("unexpected %q in output:\n%s", unwanted, output)
				}
			}
		})
	}
}

func TestIsolationRuntimeReportsLastObservation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		last     string
		want     string
		unwanted string
	}{
		{name: "last failure", last: "failure", want: "snapshot check guest-containerd failed (exit 8)", unwanted: "Snapshot mismatch:"},
		{name: "last mismatch", last: "mismatch", want: "observed mode=container version=$'1.35.0\\r'", unwanted: "first probe failure"},
		{name: "last empty", last: "empty", want: "Snapshot exit=0 with empty stdout (0 bytes)", unwanted: "Snapshot mismatch:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output := runShellResult(t, []string{
				"SCRIPT=" + isolationScript(t),
				"ROOT=" + t.TempDir(),
				"LAST=" + tc.last,
			}, `
source "$SCRIPT"
log_error() { echo "$*" >&2; }
log_info() { echo "$*"; }
_isolation_runtime_diagnostics() { echo diagnostics; }
_isolation_snapshot() {
  if [[ ! -e "$ROOT/polled" ]]; then
    touch "$ROOT/polled"
    echo 'first probe failure' >&2
    return 3
  fi
  if [[ "$LAST" == failure ]]; then
    echo 'snapshot check guest-containerd failed (exit 8)' >&2
    return 8
  fi
  if [[ "$LAST" == empty ]]; then return 0; fi
  printf 'container|kube2|1.35.0\r|settings|location\n'
}
unset SECONDS
SECONDS=0
sleep() {
  if [[ -e "$ROOT/slept" ]]; then SECONDS=100; else touch "$ROOT/slept"; fi
}
E2E_NODE_JOIN_TIMEOUT=10
E2E_KUBERNETES_VERSION=1.35.0
_isolation_wait_runtime vm container
`, false)
			if !strings.Contains(output, tc.want) || strings.Contains(output, tc.unwanted) || !strings.Contains(output, "diagnostics") {
				t.Fatalf("unexpected last observation: %s", output)
			}
		})
	}
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
