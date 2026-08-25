package e2e_test

import (
	"context"
	"embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Embedding the scripts makes Go's test cache invalidate on shell-only changes.
//
//go:embed run.sh lib/bootstrap-rbac-migration.sh lib/common.sh lib/cleanup.sh lib/controller.sh lib/node-join-arc.sh lib/runner.sh infra/*.bicep infra/modules/*.bicep
var e2eScripts embed.FS

func TestRunnerCleanupIsScopedToCurrentAttempt(t *testing.T) {
	t.Parallel()

	script, err := e2eScripts.ReadFile("lib/runner.sh")
	if err != nil {
		t.Fatalf("read embedded runner script: %v", err)
	}
	text := string(script)
	for _, required := range []string{
		`local work_dir="${E2E_WORK_DIR:-}"`,
		`^/tmp/aks-flex-node-e2e(-[A-Za-z0-9][A-Za-z0-9._-]*)?$`,
		`rm -rf -- "${work_dir}"`,
		`Refusing to clean unexpected E2E work directory`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("runner cleanup is missing attempt-scoped guard %q", required)
		}
	}
	if strings.Contains(text, `find /tmp`) || strings.Contains(text, `-name 'aks-flex-node-e2e-*'`) {
		t.Error("runner cleanup still removes work directories owned by other attempts")
	}
}

func TestRunnerCleanupRejectsUnsafeWorkDirectories(t *testing.T) {
	t.Parallel()

	runnerScript := e2eScriptPath(t, "lib", "runner.sh")
	tests := map[string]func(string) string{
		"unexpected parent": func(targetDir string) string { return targetDir },
		"parent traversal":  func(targetDir string) string { return "/tmp/.." + targetDir },
	}
	for name, workDirPath := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			targetDir := filepath.Join(t.TempDir(), "aks-flex-node-e2e-test")
			if err := os.MkdirAll(targetDir, 0o700); err != nil {
				t.Fatalf("create protected directory: %v", err)
			}
			sentinel := filepath.Join(targetDir, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatalf("write protected sentinel: %v", err)
			}
			workDir := workDirPath(targetDir)
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
sudo() { return 1; }
go() { return 0; }
if cleanup_runner_workspace; then
  printf 'RESULT=unexpected-success\n'
else
  printf 'RESULT=rejected\n'
fi
`
			output, err := runBash(t, script, workDir, runnerScript)
			if err != nil {
				t.Fatalf("runner cleanup safety harness failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "RESULT=rejected") {
				t.Fatalf("runner cleanup accepted unsafe work directory %q:\n%s", workDir, output)
			}
			if _, err := os.Stat(sentinel); err != nil {
				t.Fatalf("runner cleanup removed data outside its allowed path: %v", err)
			}
		})
	}
}

func TestBicepModuleDeploymentNamesAreUniquePerRun(t *testing.T) {
	t.Parallel()

	mainBicep, err := e2eScripts.ReadFile("infra/main.bicep")
	if err != nil {
		t.Fatalf("read embedded main.bicep: %v", err)
	}
	for _, moduleName := range []string{"msi", "token", "offline", "kubeadm", "arc"} {
		want := "name: 'deploy-vm-" + moduleName + "-${nameSuffix}'"
		if !strings.Contains(string(mainBicep), want) {
			t.Errorf("VM module %q does not use a per-run deployment name %q", moduleName, want)
		}
	}
}

func TestLoadConfigTreatsEmptyOptionalValuesAsImplicit(t *testing.T) {
	t.Parallel()

	commonScript := e2eScriptPath(t, "lib", "common.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
E2E_RESOURCE_GROUP=test-rg
E2E_LOCATION=test-location
AZURE_SUBSCRIPTION_ID=test-subscription
AZURE_TENANT_ID=test-tenant
E2E_NAME_SUFFIX=test
E2E_KUBERNETES_VERSION=
E2E_TARGET_AGENT_POOL_NAME=
E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME=
load_config >/dev/null
printf 'RESULT=%s|%s|%s|%s|%s|%s\n' \
  "${_E2E_KUBERNETES_VERSION_EXPLICIT}" "${E2E_KUBERNETES_VERSION}" \
  "${_E2E_TARGET_AGENT_POOL_NAME_EXPLICIT}" "${E2E_TARGET_AGENT_POOL_NAME}" \
  "${_E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME_EXPLICIT}" "${E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME}"
`
	output, err := runBash(t, script, t.TempDir(), commonScript)
	if err != nil {
		t.Fatalf("load config failed: %v\n%s", err, output)
	}
	const want = "RESULT=0|1.35.0|0|aksflexnodes|0|aksflexnodes\n"
	if !strings.Contains(string(output), want) {
		t.Fatalf("empty optional values were treated as explicit: got %q, want %q", output, want)
	}
}

func TestFullE2ECleanupFailureIsAggregated(t *testing.T) {
	t.Parallel()

	runScript, err := e2eScripts.ReadFile("run.sh")
	if err != nil {
		t.Fatalf("read embedded run.sh: %v", err)
	}
	if !strings.Contains(string(runScript), "cleanup || exit_code=1") {
		t.Fatal("full E2E does not aggregate cleanup failure into its final result")
	}
}

func TestRestoreKubernetesVersionFromState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		configuredVersion string
		explicit          string
		persistedVersion  string
		liveVersion       string
		liveSystemVersion string
		liveFlexVersion   string
		lookupFails       string
		omitSubscription  bool
		wantVersion       string
		wantError         string
	}{
		{
			name:              "restores persisted version when unset",
			configuredVersion: "1.35.0",
			explicit:          "0",
			persistedVersion:  "1.34.9",
			liveVersion:       "1.34.9",
			wantVersion:       "1.34.9",
		},
		{
			name:              "accepts matching explicit version",
			configuredVersion: "1.34.9",
			explicit:          "1",
			persistedVersion:  "1.34.9",
			liveVersion:       "1.34.9",
			wantVersion:       "1.34.9",
		},
		{
			name:              "rejects mismatched explicit version",
			configuredVersion: "1.35.0",
			explicit:          "1",
			persistedVersion:  "1.34.9",
			liveVersion:       "1.34.9",
			wantError:         "existing cluster state records 1.34.9",
		},
		{
			name:              "rejects invalid persisted version",
			configuredVersion: "1.35.0",
			explicit:          "0",
			persistedVersion:  "1.34",
			liveVersion:       "1.34",
			wantError:         "must be an exact x.y.z patch version",
		},
		{
			name:              "recovers version from legacy state",
			configuredVersion: "1.35.0",
			explicit:          "0",
			liveVersion:       "1.34.9",
			wantVersion:       "1.34.9",
		},
		{
			name:              "rejects stale persisted version",
			configuredVersion: "1.34.9",
			explicit:          "0",
			persistedVersion:  "1.34.9",
			liveVersion:       "1.35.0",
			wantError:         "live cluster is 1.35.0",
		},
		{
			name:              "fails closed when live lookup fails",
			configuredVersion: "1.35.0",
			explicit:          "0",
			persistedVersion:  "1.35.0",
			lookupFails:       "1",
			wantError:         "Cannot determine the live Kubernetes version",
		},
		{
			name:              "rejects control plane and pool skew",
			configuredVersion: "1.34.9",
			explicit:          "0",
			persistedVersion:  "1.34.9",
			liveVersion:       "1.34.9",
			liveSystemVersion: "1.34.9",
			liveFlexVersion:   "1.35.0",
			wantError:         "AKS version skew",
		},
		{
			name:              "requires persisted subscription",
			configuredVersion: "1.35.0",
			explicit:          "0",
			persistedVersion:  "1.35.0",
			omitSubscription:  true,
			wantError:         "cluster and subscription",
		},
	}

	commonScript := e2eScriptPath(t, "lib", "common.sh")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			liveSystemVersion := tt.liveSystemVersion
			if liveSystemVersion == "" {
				liveSystemVersion = tt.liveVersion
			}
			liveFlexVersion := tt.liveFlexVersion
			if liveFlexVersion == "" {
				liveFlexVersion = tt.liveVersion
			}
			subscription := "test-subscription"
			if tt.omitSubscription {
				subscription = ""
			}

			script := `
set -euo pipefail
unset AZURE_SUBSCRIPTION_ID
E2E_WORK_DIR="$1"
source "$2"
E2E_KUBERNETES_VERSION="$3"
_E2E_KUBERNETES_VERSION_EXPLICIT="$4"
LIVE_VERSION="$6"
LIVE_SYSTEM_VERSION="$7"
LIVE_FLEX_VERSION="$8"
LOOKUP_FAILS="$9"
SUBSCRIPTION="${10}"
az() {
  [[ "${LOOKUP_FAILS}" != "1" ]] || return 1
  if [[ "$1 $2" == "aks show" ]]; then
    [[ "$*" == *"--subscription test-subscription"* ]] || {
      echo 'aks lookup omitted persisted subscription' >&2
      return 1
    }
    jq -n --arg version "${LIVE_VERSION}" \
      '{id: "/subscriptions/test-subscription/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-aks", version: $version}'
  elif [[ "$1" == "rest" && "$*" == *'/agentPools/system?'* ]]; then
    printf '%s\n' "${LIVE_SYSTEM_VERSION}"
  elif [[ "$1" == "rest" ]]; then
    printf '%s\n' "${LIVE_FLEX_VERSION}"
  else
    return 1
  fi
}
state_set resource_group test-rg
state_set cluster_name test-aks
if [[ -n "${SUBSCRIPTION}" ]]; then
  state_set subscription_id "${SUBSCRIPTION}"
fi
if [[ -n "$5" ]]; then
  state_set kubernetes_version "$5"
fi
restore_kubernetes_version_from_state
printf 'RESULT=%s\n' "${E2E_KUBERNETES_VERSION}"
`
			output, err := runBash(t, script, t.TempDir(), commonScript,
				tt.configuredVersion, tt.explicit, tt.persistedVersion, tt.liveVersion,
				liveSystemVersion, liveFlexVersion, tt.lookupFails, subscription)
			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got success:\n%s", tt.wantError, output)
				}
				if !strings.Contains(string(output), tt.wantError) {
					t.Fatalf("error output %q does not contain %q", output, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("restore script failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "RESULT="+tt.wantVersion+"\n") {
				t.Fatalf("output %q does not contain restored version %q", output, tt.wantVersion)
			}
		})
	}
}

func TestStateRequiresVerifiedCleanupBeforeReplacement(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		stateJSON string
		wantAllow bool
	}{
		{name: "missing state", wantAllow: true},
		{name: "empty state", stateJSON: `{}`, wantAllow: true},
		{name: "verified cleanup string", stateJSON: `{"deployment_name":"e2e-old","lifecycle":"cleaned","cleanup_complete":"true"}`, wantAllow: true},
		{name: "verified cleanup boolean", stateJSON: `{"deployment_name":"e2e-old","lifecycle":"cleaned","cleanup_complete":true}`, wantAllow: true},
		{name: "same active deployment", stateJSON: `{"deployment_name":"e2e-new","lifecycle":"provisioning"}`},
		{name: "active prior deployment", stateJSON: `{"deployment_name":"e2e-old","lifecycle":"ready"}`},
		{name: "unknown legacy deployment", stateJSON: `{"cluster_name":"old-aks"}`},
		{name: "invalid json", stateJSON: `{not-json`},
		{name: "non-object json", stateJSON: `[]`},
	}

	commonScript := e2eScriptPath(t, "lib", "common.sh")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workDir := t.TempDir()
			statePath := filepath.Join(workDir, "state.json")
			if tt.stateJSON != "" {
				if err := os.WriteFile(statePath, []byte(tt.stateJSON), 0o600); err != nil {
					t.Fatalf("write initial state: %v", err)
				}
			}
			before, _ := os.ReadFile(statePath)

			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
next_state="$(jq -n '{deployment_name: "e2e-new", lifecycle: "provisioning"}')"
if state_begin_deployment "${next_state}"; then
  printf 'DECISION=ALLOWED\n'
else
  printf 'DECISION=REJECTED\n'
fi
`
			output, err := runBash(t, script, workDir, commonScript)
			if err != nil {
				t.Fatalf("state guard script failed: %v\n%s", err, output)
			}
			gotAllow := strings.Contains(string(output), "DECISION=ALLOWED\n")
			if gotAllow != tt.wantAllow {
				t.Fatalf("allow = %t, want %t; output:\n%s", gotAllow, tt.wantAllow, output)
			}
			after, readErr := os.ReadFile(statePath)
			if readErr != nil {
				t.Fatalf("read resulting state: %v", readErr)
			}
			if !tt.wantAllow && string(after) != string(before) {
				t.Fatalf("rejected replacement mutated state: before=%q after=%q", before, after)
			}
			if tt.wantAllow && !strings.Contains(string(after), `"deployment_name": "e2e-new"`) {
				t.Fatalf("allowed replacement did not install new state: %s", after)
			}
			info, statErr := os.Stat(statePath)
			if statErr != nil {
				t.Fatalf("stat state file: %v", statErr)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("state file mode = %o, want 600", got)
			}
		})
	}
}

func TestConcurrentStateWritesArePrivateAndComplete(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "state.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write initial state: %v", err)
	}
	commonScript := e2eScriptPath(t, "lib", "common.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
for i in $(seq 1 40); do
  state_set "key_${i}" "${i}" &
done
wait
jq -e 'length == 40' "${E2E_STATE_FILE}"
`
	if output, err := runBash(t, script, workDir, commonScript); err != nil {
		t.Fatalf("concurrent state script failed: %v\n%s", err, output)
	}

	info, err := os.Stat(filepath.Join(workDir, "state.json"))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state file mode = %o, want 600", got)
	}
}

func TestStateWriteFailurePreservesExistingState(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	statePath := filepath.Join(workDir, "state.json")
	const invalidState = `{not-json`
	if err := os.WriteFile(statePath, []byte(invalidState), 0o600); err != nil {
		t.Fatalf("write invalid state: %v", err)
	}
	commonScript := e2eScriptPath(t, "lib", "common.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
if state_set test value; then
  echo 'unexpected success' >&2
  exit 1
fi
`
	if output, err := runBash(t, script, workDir, commonScript); err != nil {
		t.Fatalf("failure-path script failed: %v\n%s", err, output)
	}
	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if string(after) != invalidState {
		t.Fatalf("failed state update replaced recoverable state: %q", after)
	}
}

func TestStateDumpRedactsSecrets(t *testing.T) {
	t.Parallel()

	commonScript := e2eScriptPath(t, "lib", "common.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
state_set kubeadm_bootstrap_token abcdef.0123456789abcdef
state_set token_vm_ip 192.0.2.1
state_dump
`
	output, err := runBash(t, script, t.TempDir(), commonScript)
	if err != nil {
		t.Fatalf("state dump script failed: %v\n%s", err, output)
	}
	if strings.Contains(string(output), "abcdef.0123456789abcdef") {
		t.Fatalf("state dump exposed bootstrap token: %s", output)
	}
	if !strings.Contains(string(output), "192.0.2.1") {
		t.Fatalf("state dump unexpectedly redacted non-secret metadata: %s", output)
	}
}

func TestArcRoleAssignmentIsPersistedOnlyAfterVerification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		mode              string
		initiallyVerified bool
		wantSuccess       bool
		wantCreate        bool
	}{
		{name: "existing assignment", mode: "visible", wantSuccess: true},
		{name: "delayed list visibility", mode: "delayed", wantSuccess: true, wantCreate: true},
		{name: "wrong principal returned by create", mode: "wrong-principal", wantCreate: true},
		{name: "wrong scope returned by create", mode: "wrong-scope", wantCreate: true},
		{name: "list visibility timeout clears stale state", mode: "timeout", initiallyVerified: true, wantCreate: true},
		{name: "query failure clears stale state without creating", mode: "query-error", initiallyVerified: true},
	}

	arcScript := e2eScriptPath(t, "lib", "node-join-arc.sh")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			callLog := filepath.Join(workDir, "az-calls")
			listCount := filepath.Join(workDir, "list-count")
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
LIST_COUNT="$4"
MODE="$5"
INITIALLY_VERIFIED="$6"
PRINCIPAL_ID=11111111-1111-1111-1111-111111111111
ROLE_ID=ed7f3fbd-7b88-4dd4-9017-9adb7ce333f8
CLUSTER_ID=/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-aks
SUBSCRIPTION_ID=test-subscription
role_record() {
  jq -n --arg principal "$1" --arg scope "$2" --arg role "${ROLE_ID}" '{
    id: "/subscriptions/test/providers/Microsoft.Authorization/roleAssignments/test-assignment",
    principalId: $principal,
    roleDefinitionId: ("/subscriptions/test/providers/Microsoft.Authorization/roleDefinitions/" + $role),
    scope: $scope
  }'
}
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  if [[ "$1 $2 $3" == "role assignment list" ]]; then
    if [[ "${MODE}" == "query-error" ]]; then
      return 1
    fi
    count="$(cat "${LIST_COUNT}" 2>/dev/null || printf '0\n')"
    count=$((count + 1))
    printf '%s\n' "${count}" > "${LIST_COUNT}"
    if [[ "${MODE}" == "visible" || ("${MODE}" == "delayed" && "${count}" -ge 3) ]]; then
      role_record "${PRINCIPAL_ID}" "${CLUSTER_ID}" | jq -s '.'
    else
      printf '[]\n'
    fi
  elif [[ "$1 $2 $3" == "role assignment create" ]]; then
    principal="${PRINCIPAL_ID}"
    scope="${CLUSTER_ID}"
    [[ "${MODE}" != "wrong-principal" ]] || principal=22222222-2222-2222-2222-222222222222
    [[ "${MODE}" != "wrong-scope" ]] || scope="${CLUSTER_ID}/agentPools/other"
    role_record "${principal}" "${scope}"
  else
    return 1
  fi
}
sleep() { :; }
if [[ "${INITIALLY_VERIFIED}" == "1" ]]; then
  state_set arc_role_assigned true
fi
if _ensure_arc_role_assignment "${PRINCIPAL_ID}" "${CLUSTER_ID}" "${SUBSCRIPTION_ID}"; then
  printf 'RESULT=success\n'
else
  printf 'RESULT=failure\n'
fi
printf 'STATE=%s\n' "$(state_get arc_role_assigned)"
`
			output, err := runBash(t, script, workDir, arcScript, callLog, listCount, tt.mode, boolString(tt.initiallyVerified))
			if err != nil {
				t.Fatalf("Arc role-assignment harness failed: %v\n%s", err, output)
			}
			wantResult := "RESULT=failure\n"
			wantState := "STATE=false\n"
			if tt.wantSuccess {
				wantResult = "RESULT=success\n"
				wantState = "STATE=true\n"
			}
			if !strings.Contains(string(output), wantResult) || !strings.Contains(string(output), wantState) {
				t.Fatalf("Arc role-assignment result did not match expectations:\n%s", output)
			}
			calls, readErr := os.ReadFile(callLog)
			if readErr != nil {
				t.Fatalf("read Azure CLI calls: %v", readErr)
			}
			gotCreate := strings.Contains(string(calls), "role assignment create")
			if gotCreate != tt.wantCreate {
				t.Fatalf("role assignment create = %t, want %t; calls:\n%s", gotCreate, tt.wantCreate, calls)
			}
			for _, required := range []string{
				"--assignee-object-id 11111111-1111-1111-1111-111111111111",
				"--role ed7f3fbd-7b88-4dd4-9017-9adb7ce333f8",
				"--scope /subscriptions/test/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-aks",
				"--subscription test-subscription",
			} {
				if !strings.Contains(string(calls), required) {
					t.Errorf("Azure CLI calls omitted %q:\n%s", required, calls)
				}
			}
		})
	}
}

func TestAKSContributorRoleDefinitionAllowsBootstrapDataAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		actions    string
		notActions string
		wantAllow  bool
	}{
		{name: "exact action", actions: `["Microsoft.ContainerService/managedClusters/agentPools/listBootstrapData/action"]`, notActions: `[]`, wantAllow: true},
		{name: "managed cluster wildcard", actions: `["Microsoft.ContainerService/managedClusters/*"]`, notActions: `[]`, wantAllow: true},
		{name: "global wildcard", actions: `["*"]`, notActions: `[]`, wantAllow: true},
		{name: "explicit exclusion", actions: `["Microsoft.ContainerService/managedClusters/*"]`, notActions: `["Microsoft.ContainerService/managedClusters/agentPools/listBootstrapData/action"]`},
		{name: "unrelated action", actions: `["Microsoft.Compute/*"]`, notActions: `[]`},
	}

	arcScript := e2eScriptPath(t, "lib", "node-join-arc.sh")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
ACTIONS="$3"
NOT_ACTIONS="$4"
CALL_LOG="$5"
az() {
  printf '%s\n' "$*" > "${CALL_LOG}"
  [[ "$1 $2 $3" == "role definition show" ]] || return 1
  jq -n --argjson actions "${ACTIONS}" --argjson notActions "${NOT_ACTIONS}" \
    '{permissions: [{actions: $actions, notActions: $notActions}]}'
}
if _aks_contributor_allows_bootstrap_data test-subscription; then
  printf 'RESULT=allowed\n'
else
  printf 'RESULT=denied\n'
fi
`
			workDir := t.TempDir()
			callLog := filepath.Join(workDir, "az-call")
			output, err := runBash(t, script, workDir, arcScript, tt.actions, tt.notActions, callLog)
			if err != nil {
				t.Fatalf("role-definition harness failed: %v\n%s", err, output)
			}
			gotAllow := strings.Contains(string(output), "RESULT=allowed\n")
			if gotAllow != tt.wantAllow {
				t.Fatalf("allowed = %t, want %t; output:\n%s", gotAllow, tt.wantAllow, output)
			}
			calls, readErr := os.ReadFile(callLog)
			if readErr != nil {
				t.Fatalf("read role-definition call: %v", readErr)
			}
			for _, required := range []string{
				"role definition show",
				"--id /subscriptions/test-subscription/providers/Microsoft.Authorization/roleDefinitions/ed7f3fbd-7b88-4dd4-9017-9adb7ce333f8",
				"--subscription test-subscription",
			} {
				if !strings.Contains(string(calls), required) {
					t.Errorf("role-definition lookup omitted %q: %s", required, calls)
				}
			}
		})
	}
}

func TestArcBootstrapFetchReportsBoundedAuthorizationDiagnostics(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	arcScript := e2eScriptPath(t, "lib", "node-join-arc.sh")
	fetchCount := filepath.Join(workDir, "fetch-count")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
FETCH_COUNT="$3"
E2E_BINARY=/tmp/test-binary
E2E_BOOTSTRAP_DATA_AGENT_POOL_NAME=flex
PRINCIPAL_ID=11111111-1111-1111-1111-111111111111
ROLE_ID=ed7f3fbd-7b88-4dd4-9017-9adb7ce333f8
CLUSTER_ID=/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-aks
ARC_ID=/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.HybridCompute/machines/test-arc
SUBSCRIPTION_ID=test-subscription
remote_copy() { :; }
remote_exec() {
  if [[ "$*" == *"fetch-bootstrap-data"* ]]; then
    count="$(cat "${FETCH_COUNT}" 2>/dev/null || printf '0\n')"
    printf '%s\n' "$((count + 1))" > "${FETCH_COUNT}"
    printf '%s\n' 'Command execution failed: fetch bootstrap data returned HTTP status 403: error.code="AuthorizationFailed", error.message="role assignment has not propagated", x-ms-request-id="request-123", x-ms-correlation-request-id="correlation-456"' >&2
    printf '%s\n' 'Bearer TOP_SECRET_TOKEN_MUST_NOT_APPEAR' >&2
    return 1
  fi
  if [[ "$*" == *"himdsd_active"* ]]; then
    printf 'true|true\n'
  fi
}
az() {
  if [[ "$1 $2 $3" == "role assignment list" ]]; then
    jq -n --arg principal "${PRINCIPAL_ID}" --arg role "${ROLE_ID}" --arg scope "${CLUSTER_ID}" \
      '[{principalId: $principal, roleDefinitionId: ("/subscriptions/test/providers/Microsoft.Authorization/roleDefinitions/" + $role), scope: $scope}]'
  elif [[ "$1 $2 $3" == "role definition show" ]]; then
    printf '%s\n' '{"permissions":[{"actions":["Microsoft.ContainerService/managedClusters/*"],"notActions":[]}]}'
  elif [[ "$1 $2" == "resource show" ]]; then
    printf '%s\n' "${PRINCIPAL_ID}"
  else
    return 1
  fi
}
sleep() { :; }
if _fetch_arc_bootstrap_config 192.0.2.1 10.0.0.4 test-node "${CLUSTER_ID}" "${E2E_WORK_DIR}/config.json" "${ARC_ID}" "${PRINCIPAL_ID}" "${SUBSCRIPTION_ID}"; then
  printf 'RESULT=unexpected-success\n'
else
  printf 'RESULT=expected-failure\n'
fi
printf 'FETCHES=%s\n' "$(cat "${FETCH_COUNT}")"
`
	output, err := runBash(t, script, workDir, arcScript, fetchCount)
	if err != nil {
		t.Fatalf("Arc bootstrap diagnostic harness failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, expected := range []string{
		"RESULT=expected-failure",
		"FETCHES=60",
		`error.code="AuthorizationFailed"`,
		`x-ms-request-id="request-123"`,
		`x-ms-correlation-request-id="correlation-456"`,
		"roleAssignmentVisible=true roleAllowsListBootstrapData=true arcPrincipalMatches=true himdsdActive=true azcmagentConnected=true",
		"still HTTP 403 despite a verified assignment",
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("diagnostic output omitted %q:\n%s", expected, output)
		}
	}
	for _, attempt := range []string{"1", "10", "20", "30", "40", "50", "60"} {
		marker := "Arc authorization diagnostics (" + attempt + "/60)"
		if strings.Count(text, marker) != 1 {
			t.Errorf("diagnostic marker %q count = %d, want 1", marker, strings.Count(text, marker))
		}
	}
	if got := strings.Count(text, "Arc authorization diagnostics ("); got != 7 {
		t.Errorf("authorization diagnostics emitted %d times, want 7", got)
	}
	if strings.Contains(text, "TOP_SECRET_TOKEN_MUST_NOT_APPEAR") {
		t.Fatalf("diagnostics exposed the discarded token: %s", output)
	}
}

func TestCancelDeploymentWaitsForTerminalOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		initialState  string
		terminalState string
		cancelFails   string
		listFails     string
		wantCancel    bool
		wantError     bool
	}{
		{name: "running deployment", initialState: "Running", terminalState: "Canceled", wantCancel: true},
		{name: "already succeeded", initialState: "Succeeded", terminalState: "Succeeded"},
		{name: "cancel races natural completion", initialState: "Running", terminalState: "Succeeded", cancelFails: "1", wantCancel: true},
		{name: "query failure", listFails: "1", wantError: true},
	}

	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			workDir := t.TempDir()
			callLog := filepath.Join(workDir, "az-calls.log")
			marker := filepath.Join(workDir, "deployment-terminal")
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
AZ_MARKER="$4"
INITIAL_STATE="$5"
TERMINAL_STATE="$6"
CANCEL_FAILS="$7"
LIST_FAILS="$8"
E2E_CLEANUP_TIMEOUT=2
E2E_CLEANUP_POLL_INTERVAL=0.01
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  if [[ "$1 $2 $3" == "deployment group list" ]]; then
    [[ "${LIST_FAILS}" != "1" ]] || return 1
    state="${INITIAL_STATE}"
    [[ ! -f "${AZ_MARKER}" ]] || state="${TERMINAL_STATE}"
    jq -n --arg state "${state}" '[{name:"e2e-test",properties:{provisioningState:$state}}]'
  elif [[ "$1 $2 $3" == "deployment group cancel" ]]; then
    : > "${AZ_MARKER}"
    [[ "${CANCEL_FAILS}" != "1" ]]
  elif [[ "$1 $2 $3 $4" == "deployment operation group list" ]]; then
    printf '[{"properties":{"provisioningState":"Succeeded"}}]\n'
  else
    return 1
  fi
}
if _cancel_active_deployment test-rg e2e-test test-subscription; then
  printf 'RESULT=success\n'
else
  printf 'RESULT=error\n'
fi
`
			output, err := runBash(t, script, workDir, cleanupScript, callLog, marker,
				tt.initialState, tt.terminalState, tt.cancelFails, tt.listFails)
			if err != nil {
				t.Fatalf("cancellation script failed: %v\n%s", err, output)
			}
			gotError := strings.Contains(string(output), "RESULT=error\n")
			if gotError != tt.wantError {
				t.Fatalf("error = %t, want %t; output:\n%s", gotError, tt.wantError, output)
			}
			calls, readErr := os.ReadFile(callLog)
			if readErr != nil {
				t.Fatalf("read az call log: %v", readErr)
			}
			gotCancel := strings.Contains(string(calls), "deployment group cancel")
			if gotCancel != tt.wantCancel {
				t.Fatalf("cancel call = %t, want %t; calls:\n%s", gotCancel, tt.wantCancel, calls)
			}
			if !tt.wantError {
				operationIndex := strings.Index(string(calls), "deployment operation group list")
				if operationIndex < 0 {
					t.Fatalf("deployment operations were not verified:\n%s", calls)
				}
				if tt.wantCancel && operationIndex < strings.Index(string(calls), "deployment group cancel") {
					t.Fatalf("operations were checked before cancellation:\n%s", calls)
				}
			}
		})
	}
}

func TestCleanupIsIdempotentAndWaitsForDependencies(t *testing.T) {
	t.Parallel()

	workDir, statePath, callLog, output, err := runCleanup(t, cleanupOptions{runTwice: true})
	_ = workDir
	if err != nil {
		t.Fatalf("cleanup failed: %v\n%s", err, output)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if !strings.Contains(string(state), `"cleanup_complete": "true"`) ||
		!strings.Contains(string(state), `"lifecycle": "cleaned"`) {
		t.Fatalf("cleanup did not record verified completion: %s", state)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	operationIndex := strings.Index(string(calls), "deployment operation group list")
	deleteIndex := strings.Index(string(calls), "vm delete")
	if operationIndex < 0 || deleteIndex <= operationIndex {
		t.Fatalf("resource deletion began before deployment operations were terminal:\n%s", calls)
	}
	if strings.Count(string(calls), "resource list") < 6 {
		t.Fatalf("cleanup did not perform repeated tagged and exact inventories across two runs:\n%s", calls)
	}
}

func TestCleanupResidualPreventsCleanState(t *testing.T) {
	t.Parallel()

	_, statePath, _, output, err := runCleanup(t, cleanupOptions{leaveCluster: true})
	if err != nil {
		t.Fatalf("cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") {
		t.Fatalf("cleanup unexpectedly accepted residual resource:\n%s", output)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) ||
		strings.Contains(string(state), `"lifecycle": "cleaned"`) {
		t.Fatalf("failed cleanup was marked clean: %s", state)
	}
}

func TestCleanupUsesExactNamesWithoutRunTag(t *testing.T) {
	t.Parallel()

	_, _, callLog, output, err := runCleanup(t, cleanupOptions{noRunTags: true})
	if err != nil {
		t.Fatalf("cleanup without tags failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("cleanup without tags did not succeed:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if strings.Contains(string(calls), "--tag github-run=") {
		t.Fatalf("cleanup unexpectedly used an absent run tag:\n%s", calls)
	}
	if !strings.Contains(string(calls), "resource list --resource-group test-rg --subscription test-subscription --output json") {
		t.Fatalf("cleanup skipped exact-name verification:\n%s", calls)
	}
}

func TestCleanupUsesPersistedSubscriptionWithoutEnvironment(t *testing.T) {
	t.Parallel()

	_, _, callLog, output, err := runCleanup(t, cleanupOptions{unsetSubscriptionEnv: true})
	if err != nil {
		t.Fatalf("cleanup without subscription environment failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("cleanup did not use the persisted subscription:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if !strings.Contains(string(calls), "--subscription test-subscription") {
		t.Fatalf("cleanup did not pass the persisted subscription to Azure CLI:\n%s", calls)
	}
}

func TestCleanupHandlesLegacyStateWithoutVMNames(t *testing.T) {
	t.Parallel()

	_, statePath, _, output, err := runCleanup(t, cleanupOptions{noRunTags: true, blankVMNames: true})
	if err != nil {
		t.Fatalf("legacy partial-state cleanup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("legacy partial-state cleanup did not succeed:\n%s", output)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if !strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("legacy partial-state cleanup was not marked complete: %s", state)
	}
}

func TestCleanupDeletesDetachedUntaggedLegacyOSDisk(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		runTwice:           true,
		noRunTags:          true,
		legacyDetachedDisk: true,
	})
	if err != nil {
		t.Fatalf("legacy detached OS-disk cleanup failed: %v\n%s", err, output)
	}
	if strings.Count(string(output), "RESULT=success") != 2 {
		t.Fatalf("legacy detached OS-disk cleanup was not idempotent:\n%s", output)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if !strings.Contains(string(state), `"cleanup_complete": "true"`) ||
		!strings.Contains(string(state), `"lifecycle": "cleaned"`) {
		t.Fatalf("legacy detached OS-disk cleanup did not record completion: %s", state)
	}

	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "resource delete --ids /own-legacy-disk") {
		t.Fatalf("cleanup did not delete the detached untagged legacy OS disk:\n%s", calls)
	}
	for _, foreignID := range []string{"/other-attempt-disk", "/unrelated-disk"} {
		if strings.Contains(callText, "resource delete --ids "+foreignID) {
			t.Errorf("cleanup deleted foreign resource %q:\n%s", foreignID, calls)
		}
	}
}

func TestCleanupRetriesAndReportsResidualLegacyOSDisk(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		noRunTags:                true,
		legacyDetachedDisk:       true,
		legacyDiskDeletePersists: true,
	})
	if err != nil {
		t.Fatalf("cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") {
		t.Fatalf("cleanup accepted a residual legacy OS disk:\n%s", output)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) ||
		strings.Contains(string(state), `"lifecycle": "cleaned"`) {
		t.Fatalf("residual legacy OS disk was marked clean: %s", state)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if got := strings.Count(string(calls), "resource delete --ids /own-legacy-disk"); got < 4 {
		t.Fatalf("legacy OS-disk deletion was attempted %d times, want at least 4:\n%s", got, calls)
	}
}

func TestCleanupDeletesOrphanNodeResourceGroupWhenParentIsAbsent(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{parentGroupAbsent: true})
	if err != nil {
		t.Fatalf("orphan cleanup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("orphan cleanup did not succeed:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if !strings.Contains(string(calls), "group delete --name MC_aksflex-e2e-test") {
		t.Fatalf("orphan node resource group was not deleted:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if !strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("orphan cleanup did not record completion: %s", state)
	}
}

func TestCleanupReliesOnAKSDeletionForLiveNodeResourceGroup(t *testing.T) {
	t.Parallel()

	_, _, callLog, output, err := runCleanup(t, cleanupOptions{nodeGroupForbidden: true})
	if err != nil {
		t.Fatalf("live-cluster cleanup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("cleanup did not accept confirmed AKS deletion:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "aks wait --resource-group test-rg --name aks-e2e-test") {
		t.Fatalf("cleanup did not wait for confirmed AKS deletion:\n%s", calls)
	}
	for _, operation := range []string{
		"group exists --name MC_aksflex-e2e-test",
		"group delete --name MC_aksflex-e2e-test",
		"group wait --name MC_aksflex-e2e-test",
	} {
		if strings.Contains(callText, operation) {
			t.Fatalf("cleanup directly managed the live cluster's node resource group with %q:\n%s", operation, calls)
		}
	}
}

func TestCleanupDoesNotTouchNodeResourceGroupWhenAKSRemains(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		aksDeleteRemains:   true,
		nodeGroupForbidden: true,
	})
	if err != nil {
		t.Fatalf("AKS deletion failure harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "AKS cluster still exists after cleanup timeout") {
		t.Fatalf("cleanup accepted a cluster that remained after deletion:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "aks show --resource-group test-rg --name aks-e2e-test") {
		t.Fatalf("cleanup did not verify that the AKS cluster remained:\n%s", calls)
	}
	for _, operation := range []string{
		"group exists --name MC_aksflex-e2e-test",
		"group delete --name MC_aksflex-e2e-test",
		"group wait --name MC_aksflex-e2e-test",
	} {
		if strings.Contains(callText, operation) {
			t.Fatalf("cleanup touched a live cluster's node resource group with %q:\n%s", operation, calls)
		}
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("failed AKS deletion was marked clean: %s", state)
	}
}

func TestCleanupDoesNotAssumeAKSDeletionWhenWaitIsUnconfirmed(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		aksWaitUnconfirmed: true,
		nodeGroupForbidden: true,
	})
	if err != nil {
		t.Fatalf("unconfirmed AKS deletion harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Failed to confirm AKS cluster deletion") {
		t.Fatalf("cleanup accepted an unconfirmed AKS deletion:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "aks show --resource-group test-rg --name aks-e2e-test") {
		t.Fatalf("cleanup did not inspect AKS after the wait command failed:\n%s", calls)
	}
	for _, operation := range []string{
		"group exists --name MC_aksflex-e2e-test",
		"group delete --name MC_aksflex-e2e-test",
		"group wait --name MC_aksflex-e2e-test",
	} {
		if strings.Contains(callText, operation) {
			t.Fatalf("cleanup touched the node resource group after an unconfirmed AKS deletion with %q:\n%s", operation, calls)
		}
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("unconfirmed AKS deletion was marked clean: %s", state)
	}
}

func TestCleanupPropagatesOrphanNodeResourceGroupDeleteFailure(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		parentGroupAbsent:    true,
		nodeGroupDeleteFails: true,
	})
	if err != nil {
		t.Fatalf("orphan deletion failure harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Failed to delete orphaned AKS node resource group") ||
		!strings.Contains(string(output), "AuthorizationFailed") {
		t.Fatalf("cleanup swallowed the orphan node resource group deletion failure:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "group delete --name MC_aksflex-e2e-test") {
		t.Fatalf("cleanup did not attempt orphan node resource group deletion:\n%s", calls)
	}
	if strings.Contains(callText, "group wait --name MC_aksflex-e2e-test") {
		t.Fatalf("cleanup waited after orphan node resource group deletion failed:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("failed orphan deletion was marked clean: %s", state)
	}
}

func TestCleanupTreatsOrphanDeleteNotFoundAsIdempotent(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		parentGroupAbsent:       true,
		nodeGroupDeleteNotFound: true,
	})
	if err != nil {
		t.Fatalf("orphan not-found harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("cleanup did not accept explicit orphan absence:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "group delete --name MC_aksflex-e2e-test") {
		t.Fatalf("cleanup did not encounter the orphan deletion race:\n%s", calls)
	}
	if strings.Contains(callText, "group wait --name MC_aksflex-e2e-test") {
		t.Fatalf("cleanup waited after Azure reported the orphan absent:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if !strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("idempotent orphan cleanup was not marked complete: %s", state)
	}
}

func TestCleanupRejectsUnexpectedOrphanNodeResourceGroup(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{
		parentGroupAbsent:      true,
		unexpectedNodeResource: true,
	})
	if err != nil {
		t.Fatalf("orphan cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Refusing to delete unexpected AKS node resource group") {
		t.Fatalf("cleanup did not reject an unexpected orphan node resource group:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if strings.Contains(string(calls), "group delete --name production-node-rg") {
		t.Fatalf("cleanup attempted to delete an unexpected resource group:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("rejected cleanup was marked complete: %s", state)
	}
}

func TestCleanupRejectsUnexpectedLiveNodeResourceGroup(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{unexpectedLiveNodeRG: true})
	if err != nil {
		t.Fatalf("live node resource group cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Refusing to delete unexpected AKS node resource group 'production-live-node-rg'") {
		t.Fatalf("cleanup did not reject an unexpected live node resource group:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	for _, mutation := range []string{"group delete", "vm delete", "aks delete", "rest --method delete"} {
		if strings.Contains(string(calls), mutation) {
			t.Fatalf("cleanup attempted mutation %q for an unexpected live node resource group:\n%s", mutation, calls)
		}
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("rejected cleanup was marked complete: %s", state)
	}
}

func TestCleanupRejectsUnexpectedArcMachineID(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{unexpectedArcMachineID: true})
	if err != nil {
		t.Fatalf("Arc cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Refusing to delete Arc machine with an unexpected persisted resource ID") {
		t.Fatalf("cleanup did not reject an unexpected Arc machine ID:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read az calls: %v", readErr)
	}
	if strings.Contains(string(calls), "rest --method delete") {
		t.Fatalf("cleanup attempted to delete an unexpected Arc resource:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("rejected cleanup was marked complete: %s", state)
	}
}

func TestCleanupRetriesTransientAzureQueries(t *testing.T) {
	t.Parallel()

	workDir, _, callLog, output, err := runCleanup(t, cleanupOptions{transientQueryFailures: true})
	if err != nil {
		t.Fatalf("transient-query cleanup failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=success") {
		t.Fatalf("cleanup did not recover from transient Azure query failures:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if strings.Count(string(calls), "group exists") < 3 {
		t.Errorf("cleanup did not retry a failed resource-group query:\n%s", calls)
	}
	for _, marker := range []string{"group-query-failed", "resource-query-failed"} {
		failures, readErr := os.ReadFile(filepath.Join(workDir, marker))
		if readErr != nil {
			t.Fatalf("read %s: %v", marker, readErr)
		}
		if strings.TrimSpace(string(failures)) != "2" {
			t.Errorf("%s recorded %q transient failures, want 2", marker, strings.TrimSpace(string(failures)))
		}
	}
}

func TestCleanupFiltersResourceInventoryByTagWithinGroup(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	callLog := filepath.Join(workDir, "az-calls")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  printf '%s\n' '[
    {"name":"owned","tags":{"github-run":"test-run"}},
    {"name":"another-run","tags":{"github-run":"other-run"}},
    {"name":"untagged"}
  ]'
}
inventory="$(_resource_inventory test-rg test-subscription test-run 0)"
printf 'INVENTORY=%s\n' "${inventory}"
`
	output, err := runBash(t, script, workDir, cleanupScript, callLog)
	if err != nil {
		t.Fatalf("tagged resource inventory harness failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, `"name":"owned"`) ||
		strings.Contains(text, `"name":"another-run"`) ||
		strings.Contains(text, `"name":"untagged"`) {
		t.Fatalf("resource inventory was not filtered to the requested run tag:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	callText := string(calls)
	if !strings.Contains(callText, "resource list --resource-group test-rg --subscription test-subscription --output json") {
		t.Fatalf("resource inventory was not scoped to the parent resource group:\n%s", calls)
	}
	if strings.Contains(callText, "--tag") {
		t.Fatalf("resource inventory used Azure CLI's incompatible --resource-group/--tag combination:\n%s", calls)
	}
}

func TestCleanupAzureQueryFailureIncludesDiagnostic(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
E2E_CLEANUP_POLL_INTERVAL=0.01
az() {
  printf '%s\n' '(TooManyRequests) ARM throttled the cleanup query' >&2
  return 1
}
if _resource_inventory test-rg test-subscription "" 0; then
  printf 'RESULT=unexpected-success\n'
else
  printf 'RESULT=error\n'
fi
`
	output, err := runBash(t, script, workDir, cleanupScript)
	if err != nil {
		t.Fatalf("Azure query diagnostic harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") ||
		!strings.Contains(string(output), "Last Azure CLI error: (TooManyRequests) ARM throttled the cleanup query") {
		t.Fatalf("cleanup omitted the final Azure CLI diagnostic:\n%s", output)
	}
}

func TestCleanupAzureQueriesStopOnAuthorizationFailure(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"group", "inventory"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			workDir := t.TempDir()
			cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
			callLog := filepath.Join(workDir, "az-calls")
			script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
QUERY="$4"
E2E_CLEANUP_POLL_INTERVAL=0.01
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  printf '%s\n' "ERROR: Operation returned an invalid status 'Forbidden'" >&2
  return 1
}
deadline=$((SECONDS + 60))
if [[ "${QUERY}" == "group" ]]; then
  _group_exists_with_retry test-rg test-subscription "${deadline}" || true
else
  _resource_inventory test-rg test-subscription "" "${deadline}" || true
fi
printf 'CALLS=%s\n' "$(wc -l < "${AZ_CALL_LOG}")"
`
			output, err := runBash(t, script, workDir, cleanupScript, callLog, query)
			if err != nil {
				t.Fatalf("authorization failure harness failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "after 1 attempts") ||
				!strings.Contains(string(output), "invalid status 'Forbidden'") ||
				!strings.Contains(string(output), "CALLS=1") {
				t.Fatalf("authorization failure was retried or lost its diagnostic:\n%s", output)
			}
		})
	}
}

func TestCleanupAzureQueriesBoundRetriesByAttemptAndDeadline(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	callLog := filepath.Join(workDir, "az-calls")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
E2E_CLEANUP_POLL_INTERVAL=0.01
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  printf '%s\n' '(TooManyRequests) ARM throttled the cleanup query' >&2
  return 1
}
sleep() { :; }
deadline=$((SECONDS + 60))
_group_exists_with_retry test-rg test-subscription "${deadline}" || true
printf 'GROUP_CALLS=%s\n' "$(wc -l < "${AZ_CALL_LOG}")"
: > "${AZ_CALL_LOG}"
_resource_inventory test-rg test-subscription "" "${deadline}" || true
printf 'INVENTORY_CALLS=%s\n' "$(wc -l < "${AZ_CALL_LOG}")"
`
	output, err := runBash(t, script, workDir, cleanupScript, callLog)
	if err != nil {
		t.Fatalf("bounded retry harness failed: %v\n%s", err, output)
	}
	for _, expected := range []string{"GROUP_CALLS=5", "INVENTORY_CALLS=5"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("Azure query retries were not bounded by attempt count; missing %q:\n%s", expected, output)
		}
	}
}

func TestCleanupTreatsResourceGroupNotFoundAsAbsent(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	callLog := filepath.Join(workDir, "az-calls")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  printf '%s\n' '(ResourceGroupNotFound) Resource group was not found.' >&2
  return 1
}
result="$(_group_exists_with_retry test-rg test-subscription 0)"
printf 'RESULT=%s\n' "${result}"
printf 'CALLS=%s\n' "$(wc -l < "${AZ_CALL_LOG}")"
`
	output, err := runBash(t, script, workDir, cleanupScript, callLog)
	if err != nil {
		t.Fatalf("ResourceGroupNotFound harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=false") ||
		!strings.Contains(string(output), "CALLS=1") {
		t.Fatalf("ResourceGroupNotFound was not treated as successful absence:\n%s", output)
	}
}

func TestCleanupAzureQueriesHonorExpiredDeadline(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	callLog := filepath.Join(workDir, "az-calls")
	sleepLog := filepath.Join(workDir, "sleep-delay")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
SLEEP_LOG="$4"
az() { printf '%s\n' "$*" >> "${AZ_CALL_LOG}"; return 1; }
sleep() { printf '%s\n' "$1" > "${SLEEP_LOG}"; }

SECONDS=10
if _group_exists_with_retry test-rg test-subscription 5; then
  printf 'GROUP=unexpected-success\n'
else
  printf 'GROUP=expired\n'
fi
if _resource_inventory test-rg test-subscription "" 5; then
  printf 'INVENTORY=unexpected-success\n'
else
  printf 'INVENTORY=expired\n'
fi
calls=0
[[ ! -f "${AZ_CALL_LOG}" ]] || calls="$(wc -l < "${AZ_CALL_LOG}")"
printf 'CALLS=%s\n' "${calls}"

E2E_CLEANUP_POLL_INTERVAL=30
SECONDS=10
_sleep_before_azure_query_retry 11
printf 'SLEEP=%s\n' "$(<"${SLEEP_LOG}")"
`
	output, err := runBash(t, script, workDir, cleanupScript, callLog, sleepLog)
	if err != nil {
		t.Fatalf("Azure query deadline harness failed: %v\n%s", err, output)
	}
	for _, expected := range []string{"GROUP=expired", "INVENTORY=expired", "CALLS=0", "SLEEP=1"} {
		if !strings.Contains(string(output), expected) {
			t.Fatalf("Azure query deadline was not enforced; missing %q:\n%s", expected, output)
		}
	}
}

func TestCleanupQueryFailureDoesNotDeleteResources(t *testing.T) {
	t.Parallel()

	_, statePath, callLog, output, err := runCleanup(t, cleanupOptions{deploymentQueryFails: true})
	if err != nil {
		t.Fatalf("cleanup test harness failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "RESULT=error") {
		t.Fatalf("cleanup unexpectedly succeeded:\n%s", output)
	}
	calls, readErr := os.ReadFile(callLog)
	if readErr != nil {
		t.Fatalf("read az calls: %v", readErr)
	}
	if strings.Contains(string(calls), "vm delete") || strings.Contains(string(calls), "aks delete") {
		t.Fatalf("cleanup deleted resources after deployment query failure:\n%s", calls)
	}
	state, readErr := os.ReadFile(statePath)
	if readErr != nil {
		t.Fatalf("read state: %v", readErr)
	}
	if strings.Contains(string(state), `"cleanup_complete": "true"`) {
		t.Fatalf("query failure was marked clean: %s", state)
	}
}

func TestHistoricalCertificateProbeUsesPrivilegedTemporaryFile(t *testing.T) {
	t.Parallel()

	script, err := e2eScripts.ReadFile("lib/bootstrap-rbac-migration.sh")
	if err != nil {
		t.Fatalf("read embedded migration script: %v", err)
	}
	for _, required := range []string{
		`ca_file="$(sudo mktemp)"`,
		`sudo python3 <<'PY' | sudo tee "${ca_file}" >/dev/null`,
		`trap 'sudo rm -f "${ca_file}"' EXIT`,
	} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("migration certificate probe is missing %q", required)
		}
	}
}

func TestHistoricalMigrationReissuesDaemonCertificateBeforeTokenRevocation(t *testing.T) {
	t.Parallel()

	script, err := e2eScripts.ReadFile("lib/bootstrap-rbac-migration.sh")
	if err != nil {
		t.Fatalf("read embedded migration script: %v", err)
	}
	text := string(script)
	for _, required := range []string{
		`old_fingerprint="$(openssl x509 -in "${credential_path}" -outform DER`,
		`rm -rf -- "${credential_dir}"`,
		`new_fingerprint="$(openssl x509 -in "${credential_path}" -outform DER`,
		`_require_daemon_certificate_access "${vm_ip}" "${server_url}"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("historical migration is missing certificate reissuance check %q", required)
		}
	}

	migrationIndex := strings.LastIndex(text, `    --remove-legacy-node-role-binding`)
	reissueIndex := strings.LastIndex(text, `  _reissue_daemon_certificate_after_migration "${vm_ip}" "${server_url}"`)
	revokeIndex := strings.LastIndex(text, `  with_cluster_lock _revoke_historical_bootstrap_token "${config_file}"`)
	if migrationIndex < 0 || reissueIndex <= migrationIndex || revokeIndex <= reissueIndex {
		t.Fatalf("certificate reissuance must run after RBAC migration and before token revocation")
	}
}

func TestHistoricalTokenRevocationPropagatesDeleteFailure(t *testing.T) {
	t.Parallel()

	script, err := e2eScripts.ReadFile("lib/bootstrap-rbac-migration.sh")
	if err != nil {
		t.Fatalf("read embedded migration script: %v", err)
	}
	text := string(script)
	start := strings.Index(text, "_revoke_historical_bootstrap_token() {")
	if start < 0 {
		t.Fatal("historical token revocation helper is absent")
	}
	end := strings.Index(text[start:], "\n}\n")
	if end < 0 {
		t.Fatal("historical token revocation helper is malformed")
	}
	body := text[start : start+end]
	for _, required := range []string{
		`if ! kubectl delete secret "bootstrap-token-${token_id}" -n kube-system; then`,
		`log_error "Failed to revoke the historical bootstrap token"`,
		`return 1`,
	} {
		if !strings.Contains(body, required) {
			t.Fatalf("historical token revocation does not fail closed on delete errors; missing %q", required)
		}
	}
}

type cleanupOptions struct {
	runTwice                  bool
	leaveCluster              bool
	aksDeleteRemains          bool
	aksWaitUnconfirmed        bool
	parentGroupAbsent         bool
	deploymentQueryFails      bool
	noRunTags                 bool
	blankVMNames              bool
	nodeGroupForbidden        bool
	nodeGroupDeleteFails      bool
	nodeGroupDeleteNotFound   bool
	unexpectedNodeResource    bool
	unexpectedLiveNodeRG      bool
	legacyDefaultNodeResource bool
	legacyDetachedDisk        bool
	legacyDiskDeletePersists  bool
	unexpectedArcMachineID    bool
	transientQueryFailures    bool
	unsetSubscriptionEnv      bool
}

func runCleanup(t *testing.T, options cleanupOptions) (string, string, string, []byte, error) {
	t.Helper()
	workDir := t.TempDir()
	statePath := filepath.Join(workDir, "state.json")
	callLog := filepath.Join(workDir, "az-calls.log")
	nodeGroupDeleted := filepath.Join(workDir, "node-group-deleted")
	groupQueryFailed := filepath.Join(workDir, "group-query-failed")
	resourceQueryFailed := filepath.Join(workDir, "resource-query-failed")
	clusterDeleted := filepath.Join(workDir, "cluster-deleted")
	state := `{
  "resource_group": "test-rg",
  "location": "test-location",
  "subscription_id": "test-subscription",
  "deployment_name": "e2e-test",
  "run_id": "test-run",
  "name_suffix": "test",
  "lifecycle": "ready",
  "cluster_name": "aks-e2e-test",
  "node_resource_group": "MC_aksflex-e2e-test",
  "msi_vm_name": "vm-e2e-msi-test",
  "token_vm_name": "vm-e2e-token-test",
  "offline_vm_name": "vm-e2e-offline-test",
  "kubeadm_vm_name": "vm-e2e-kubeadm-test",
  "arc_vm_name": "vm-e2e-arc-test",
  "arc_machine_name": "",
  "vnet_name": "vnet-e2e-test",
  "nsg_name": "nsg-e2e-test"
}`
	if options.noRunTags {
		state = strings.Replace(state, `"run_id": "test-run"`, `"run_id": ""`, 1)
	}
	if options.blankVMNames {
		for _, name := range []string{
			"vm-e2e-msi-test",
			"vm-e2e-token-test",
			"vm-e2e-offline-test",
			"vm-e2e-kubeadm-test",
			"vm-e2e-arc-test",
		} {
			state = strings.ReplaceAll(state, name, "")
		}
	}
	if options.unexpectedNodeResource {
		state = strings.Replace(state, `"node_resource_group": "MC_aksflex-e2e-test"`, `"node_resource_group": "production-node-rg"`, 1)
	}
	if options.legacyDefaultNodeResource {
		state = strings.Replace(state, "  \"node_resource_group\": \"MC_aksflex-e2e-test\",\n", "", 1)
	}
	if options.unexpectedArcMachineID {
		state = strings.Replace(state, `"arc_machine_name": ""`, `"arc_machine_name": "vm-e2e-arc-test-connected",
  "arc_machine_id": "/subscriptions/test-subscription/resourceGroups/production-rg/providers/Microsoft.HybridCompute/machines/production-machine"`, 1)
	}
	if err := os.WriteFile(statePath, []byte(state), 0o600); err != nil {
		t.Fatalf("write cleanup state: %v", err)
	}
	cleanupScript := e2eScriptPath(t, "lib", "cleanup.sh")
	script := `
set -euo pipefail
E2E_WORK_DIR="$1"
source "$2"
AZ_CALL_LOG="$3"
NODE_GROUP_DELETED="$4"
GROUP_QUERY_FAILED="$5"
RESOURCE_QUERY_FAILED="$6"
LEAVE_CLUSTER="$7"
PARENT_GROUP_ABSENT="$8"
DEPLOYMENT_QUERY_FAILS="$9"
RUN_TWICE="${10}"
TRANSIENT_QUERY_FAILURES="${11}"
UNEXPECTED_LIVE_NODE_RG="${12}"
LEGACY_DEFAULT_NODE_RG="${13}"
LEGACY_DETACHED_DISK="${14}"
LEGACY_DISK_DELETE_PERSISTS="${15}"
UNSET_SUBSCRIPTION_ENV="${16}"
AKS_DELETE_REMAINS="${17}"
NODE_GROUP_FORBIDDEN="${18}"
NODE_GROUP_DELETE_FAILS="${19}"
CLUSTER_DELETED="${20}"
AKS_WAIT_UNCONFIRMED="${21}"
NODE_GROUP_DELETE_NOT_FOUND="${22}"
LEGACY_DISK_DELETED="${E2E_WORK_DIR}/legacy-disk-deleted"
# Keep cleanup fixtures independent from the ambient GitHub Actions run. Tests
# that need tag-based cleanup persist an explicit run_id in their state.
unset GITHUB_RUN_ID
if [[ "${UNSET_SUBSCRIPTION_ENV}" == "1" ]]; then
  unset AZURE_SUBSCRIPTION_ID
else
  AZURE_SUBSCRIPTION_ID=test-subscription
fi
E2E_SKIP_CLEANUP=0
E2E_CLEANUP_TIMEOUT=5
E2E_CLEANUP_POLL_INTERVAL=0.01
az() {
  printf '%s\n' "$*" >> "${AZ_CALL_LOG}"
  if [[ "$1 $2 $3" == "deployment group list" ]]; then
    [[ "${DEPLOYMENT_QUERY_FAILS}" != "1" ]] || return 1
    printf '[{"name":"e2e-test","properties":{"provisioningState":"Succeeded"}}]\n'
  elif [[ "$1 $2 $3 $4" == "deployment operation group list" ]]; then
    printf '[{"properties":{"provisioningState":"Succeeded"}}]\n'
  elif [[ "$1 $2" == "group exists" ]]; then
    if [[ "$*" == *"--name MC_aksflex-e2e-test"* || \
            "$*" == *"--name MC_test-rg_aks-e2e-test_test-location"* ]]; then
      if [[ "${NODE_GROUP_FORBIDDEN}" == "1" ]]; then
        printf '%s\n' "ERROR: Operation returned an invalid status 'Forbidden'" >&2
        return 1
      fi
      [[ -f "${NODE_GROUP_DELETED}" ]] && printf 'false\n' || printf 'true\n'
    elif [[ "$*" == *"--name test-rg"* ]]; then
      query_failures="$(cat "${GROUP_QUERY_FAILED}" 2>/dev/null || printf '0\n')"
      if [[ "${TRANSIENT_QUERY_FAILURES}" == "1" && "${query_failures}" -lt 2 ]]; then
        printf '%s\n' "$((query_failures + 1))" > "${GROUP_QUERY_FAILED}"
        printf '%s\n' '(TooManyRequests) transient resource-group query failure' >&2
        return 1
      fi
      [[ "${PARENT_GROUP_ABSENT}" == "1" ]] && printf 'false\n' || printf 'true\n'
    else
      printf 'false\n'
    fi
  elif [[ "$1 $2" == "group delete" ]]; then
    if [[ "${NODE_GROUP_DELETE_FAILS}" == "1" ]]; then
      printf '%s\n' '(AuthorizationFailed) The client cannot delete this resource group.' >&2
      return 1
    fi
    if [[ "${NODE_GROUP_DELETE_NOT_FOUND}" == "1" ]]; then
      printf '%s\n' '(ResourceGroupNotFound) Resource group was not found.' >&2
      return 1
    fi
    : > "${NODE_GROUP_DELETED}"
  elif [[ "$1 $2" == "group wait" ]]; then
    return 0
  elif [[ "$1 $2" == "vm list" ]]; then
    printf '[]\n'
  elif [[ "$1 $2" == "aks list" ]]; then
    if [[ -f "${CLUSTER_DELETED}" ]]; then
      printf '[]\n'
    elif [[ "${UNEXPECTED_LIVE_NODE_RG}" == "1" ]]; then
      printf '[{"name":"aks-e2e-test","nodeResourceGroup":"production-live-node-rg"}]\n'
    elif [[ "${LEGACY_DEFAULT_NODE_RG}" == "1" ]]; then
      printf '[{"name":"aks-e2e-test","location":"test-location","nodeResourceGroup":"MC_test-rg_aks-e2e-test_test-location"}]\n'
    else
      printf '[{"name":"aks-e2e-test","location":"test-location","nodeResourceGroup":"MC_aksflex-e2e-test"}]\n'
    fi
  elif [[ "$1 $2" == "aks delete" ]]; then
    [[ "${AKS_DELETE_REMAINS}" == "1" ]] || : > "${CLUSTER_DELETED}"
  elif [[ "$1 $2" == "aks wait" ]]; then
    [[ "${AKS_DELETE_REMAINS}" != "1" && "${AKS_WAIT_UNCONFIRMED}" != "1" ]]
  elif [[ "$1 $2" == "aks show" ]]; then
    if [[ "${AKS_WAIT_UNCONFIRMED}" == "1" ]]; then
      printf '%s\n' "ERROR: Operation returned an invalid status 'Forbidden'" >&2
      return 1
    fi
    [[ "${AKS_DELETE_REMAINS}" == "1" ]]
  elif [[ "$1 $2" == "resource delete" ]]; then
    if [[ "$*" == *"--ids /own-legacy-disk"* && \
          "${LEGACY_DISK_DELETE_PERSISTS}" != "1" ]]; then
      : > "${LEGACY_DISK_DELETED}"
    fi
    return 0
  elif [[ "$1 $2" == "resource list" ]]; then
    query_failures="$(cat "${RESOURCE_QUERY_FAILED}" 2>/dev/null || printf '0\n')"
    if [[ "${TRANSIENT_QUERY_FAILURES}" == "1" && "${query_failures}" -lt 2 ]]; then
      printf '%s\n' "$((query_failures + 1))" > "${RESOURCE_QUERY_FAILED}"
      printf '%s\n' '(TooManyRequests) transient resource inventory failure' >&2
      return 1
    fi
    if [[ "$*" == *"--tag "* ]]; then
      [[ "$*" == *"--output json"* ]] && printf '[]\n' || true
    elif [[ "$*" == *"--output json"* ]]; then
      resource_json='[]'
      if [[ "${LEAVE_CLUSTER}" == "1" ]]; then
        resource_json="$(jq -c '. + [{"name":"aks-e2e-test","id":"/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/aks-e2e-test"}]' <<<"${resource_json}")"
      fi
      if [[ "${LEGACY_DETACHED_DISK}" == "1" ]]; then
        resource_json="$(jq -c '. + [
          {"type":"Microsoft.Compute/disks","name":"vm-e2e-token-other_OsDisk_1_efgh","id":"/other-attempt-disk"},
          {"type":"Microsoft.Compute/disks","name":"production-disk","id":"/unrelated-disk"}
        ]' <<<"${resource_json}")"
        if [[ ! -f "${LEGACY_DISK_DELETED}" ]]; then
          resource_json="$(jq -c '. + [{"type":"Microsoft.Compute/disks","name":"vm-e2e-token-test_OsDisk_1_abcd","id":"/own-legacy-disk"}]' <<<"${resource_json}")"
        fi
      fi
      printf '%s\n' "${resource_json}"
    fi
  else
    return 0
  fi
}
run_cleanup() {
  if cleanup; then
    printf 'RESULT=success\n'
  else
    printf 'RESULT=error\n'
  fi
}
run_cleanup
if [[ "${RUN_TWICE}" == "1" ]]; then
  run_cleanup
fi
`
	output, err := runBash(t, script, workDir, cleanupScript, callLog, nodeGroupDeleted,
		groupQueryFailed, resourceQueryFailed,
		boolString(options.leaveCluster), boolString(options.parentGroupAbsent),
		boolString(options.deploymentQueryFails), boolString(options.runTwice),
		boolString(options.transientQueryFailures), boolString(options.unexpectedLiveNodeRG),
		boolString(options.legacyDefaultNodeResource), boolString(options.legacyDetachedDisk),
		boolString(options.legacyDiskDeletePersists), boolString(options.unsetSubscriptionEnv),
		boolString(options.aksDeleteRemains), boolString(options.nodeGroupForbidden),
		boolString(options.nodeGroupDeleteFails), clusterDeleted,
		boolString(options.aksWaitUnconfirmed), boolString(options.nodeGroupDeleteNotFound))
	return workDir, statePath, callLog, output, err
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func e2eScriptPath(t *testing.T, elements ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"bootstrap-rbac-migration.sh", "common.sh", "cleanup.sh", "controller.sh", "node-join-arc.sh", "runner.sh"} {
		contents, err := e2eScripts.ReadFile(filepath.ToSlash(filepath.Join("lib", name)))
		if err != nil {
			t.Fatalf("read embedded %s: %v", name, err)
		}
		path := filepath.Join(root, "lib", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create embedded script directory: %v", err)
		}
		if err := os.WriteFile(path, contents, 0o700); err != nil {
			t.Fatalf("materialize embedded %s: %v", name, err)
		}
	}
	return filepath.Join(append([]string{root}, elements...)...)
}

func runBash(t *testing.T, script string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commandArgs := append([]string{"-c", script, "e2e-script-test"}, args...)
	cmd := exec.CommandContext(ctx, "bash", commandArgs...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("shell test timed out: %v\n%s", ctx.Err(), output)
	}
	return output, err
}
