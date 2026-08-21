package scripts

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	bootstrapGroup     = "system:bootstrappers:aks-flex-node"
	legacyBindingName  = "aks-flex-node-role"
	legacyNodeRole     = "system:node"
	fakeDeleteFailure  = 37
	fakeCommandLogEnv  = "AKS_FLEX_CONFIG_TEST_COMMAND_LOG"
	fakeManifestEnv    = "AKS_FLEX_CONFIG_TEST_MANIFEST"
	fakeLegacyStateEnv = "AKS_FLEX_CONFIG_TEST_LEGACY_STATE"
	fakeDeleteExitEnv  = "AKS_FLEX_CONFIG_TEST_DELETE_EXIT"
	fakeApplyCountEnv  = "AKS_FLEX_CONFIG_TEST_APPLY_COUNT"
	fakeApplyFailAtEnv = "AKS_FLEX_CONFIG_TEST_APPLY_FAIL_AT"
)

type commandCall struct {
	name string
	args []string
}

type configScriptHarness struct {
	pythonPath     string
	scriptPath     string
	fakeBinDir     string
	commandLogPath string
	manifestPath   string
	configPath     string
	legacyState    string
	deleteExitCode int
	applyCountPath string
	applyFailAt    int
}

func TestSetupNodeRBACManifestUsesOnlyBootstrapPermissions(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	output, err := harness.runSetupNodeRBAC()
	if err != nil {
		t.Fatalf("setup-node-rbac failed: %v\n%s", err, output)
	}

	bindings := readRBACManifest(t, harness.manifestPath)
	expectedRoles := map[string]string{
		"aks-flex-node-bootstrapper":     "system:node-bootstrapper",
		"aks-flex-node-auto-approve-csr": "system:certificates.k8s.io:certificatesigningrequests:nodeclient",
	}
	if len(bindings) != len(expectedRoles) {
		t.Fatalf("RBAC manifest has %d bindings, want %d: %v", len(bindings), len(expectedRoles), bindingNames(bindings))
	}

	seen := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.APIVersion != "rbac.authorization.k8s.io/v1" || binding.Kind != "ClusterRoleBinding" {
			t.Errorf("binding %q has apiVersion/kind %q/%q, want rbac.authorization.k8s.io/v1/ClusterRoleBinding", binding.Name, binding.APIVersion, binding.Kind)
		}
		if binding.Namespace != "" {
			t.Errorf("binding %q unexpectedly has namespace %q", binding.Name, binding.Namespace)
		}
		if _, duplicate := seen[binding.Name]; duplicate {
			t.Errorf("binding %q appears more than once", binding.Name)
		}
		seen[binding.Name] = struct{}{}

		wantRole, expected := expectedRoles[binding.Name]
		if !expected {
			t.Errorf("unexpected ClusterRoleBinding %q", binding.Name)
			continue
		}
		if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != wantRole {
			t.Errorf("binding %q roleRef = %#v, want ClusterRole %q in %q", binding.Name, binding.RoleRef, wantRole, rbacv1.GroupName)
		}
		if len(binding.Subjects) != 1 {
			t.Errorf("binding %q has %d subjects, want exactly one", binding.Name, len(binding.Subjects))
			continue
		}
		subject := binding.Subjects[0]
		if subject.APIGroup != rbacv1.GroupName || subject.Kind != "Group" || subject.Name != bootstrapGroup || subject.Namespace != "" {
			t.Errorf("binding %q subject = %#v, want bootstrapper group %q", binding.Name, subject, bootstrapGroup)
		}
		if binding.RoleRef.Name == legacyNodeRole {
			t.Errorf("bootstrap group must not be bound to broad legacy role %q", legacyNodeRole)
		}
	}

	if _, found := seen[legacyBindingName]; found {
		t.Errorf("RBAC manifest still contains legacy binding %q", legacyBindingName)
	}
}

func TestSetupNodeRBACMigratesLegacyBindingIdempotently(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, 0)
	for run := 1; run <= 2; run++ {
		output, err := harness.runSetupNodeRBAC()
		if err != nil {
			t.Fatalf("setup-node-rbac run %d failed: %v\n%s", run, err, output)
		}
	}

	state, err := os.ReadFile(harness.legacyState)
	if err != nil {
		t.Fatalf("read fake legacy state: %v", err)
	}
	if got := strings.TrimSpace(string(state)); got != "absent" {
		t.Fatalf("legacy binding state = %q, want absent", got)
	}

	calls := readCommandCalls(t, harness.commandLogPath)
	applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
	if len(applyIndexes) != 2 || len(deleteIndexes) != 2 {
		t.Fatalf("kubectl apply/delete counts = %d/%d, want 2/2; calls: %s", len(applyIndexes), len(deleteIndexes), formatCalls(calls))
	}
	for i := range applyIndexes {
		if applyIndexes[i] >= deleteIndexes[i] {
			t.Errorf("run %d deletes legacy binding before applying safe RBAC; calls: %s", i+1, formatCalls(calls))
		}
		assertLegacyDeleteCall(t, calls[deleteIndexes[i]])
	}
}

func TestSetupNodeRBACFailsWhenLegacyBindingDeleteFails(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, fakeDeleteFailure)
	output, err := harness.runSetupNodeRBAC()
	if err == nil {
		t.Fatalf("setup-node-rbac succeeded when legacy binding deletion failed\n%s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("setup-node-rbac error = %T %v, want *exec.ExitError", err, err)
	}
	if got := exitErr.ExitCode(); got != fakeDeleteFailure {
		t.Fatalf("setup-node-rbac exit code = %d, want %d\n%s", got, fakeDeleteFailure, output)
	}

	state, readErr := os.ReadFile(harness.legacyState)
	if readErr != nil {
		t.Fatalf("read fake legacy state: %v", readErr)
	}
	if got := strings.TrimSpace(string(state)); got != "present" {
		t.Fatalf("legacy binding state = %q after failed deletion, want present", got)
	}

	calls := readCommandCalls(t, harness.commandLogPath)
	applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
	if len(applyIndexes) != 1 || len(deleteIndexes) != 1 {
		t.Fatalf("kubectl apply/delete counts = %d/%d, want 1/1; calls: %s", len(applyIndexes), len(deleteIndexes), formatCalls(calls))
	}
	if applyIndexes[0] >= deleteIndexes[0] {
		t.Errorf("legacy delete failure occurred before safe RBAC was applied; calls: %s", formatCalls(calls))
	}
	assertLegacyDeleteCall(t, calls[deleteIndexes[0]])
}

func TestGenerateBootstrapTokenCleansLegacyBindingBeforeMintingToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		deleteExitCode int
		wantFailure    bool
	}{
		{name: "cleanup succeeds", deleteExitCode: 0},
		{name: "cleanup fails closed", deleteExitCode: fakeDeleteFailure, wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, true, test.deleteExitCode)
			output, err := harness.runGenerateNodeConfig()
			if test.wantFailure {
				if err == nil {
					t.Fatalf("generate-node-config succeeded when legacy cleanup failed\n%s", output)
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != fakeDeleteFailure {
					t.Fatalf("generate-node-config error = %v, want exit code %d\n%s", err, fakeDeleteFailure, output)
				}
			} else if err != nil {
				t.Fatalf("generate-node-config failed: %v\n%s", err, output)
			}

			calls := readCommandCalls(t, harness.commandLogPath)
			applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
			if len(deleteIndexes) != 1 {
				t.Fatalf("kubectl delete count = %d, want 1; calls: %s", len(deleteIndexes), formatCalls(calls))
			}
			assertLegacyDeleteCall(t, calls[deleteIndexes[0]])
			if test.wantFailure {
				if len(applyIndexes) != 0 {
					t.Fatalf("token Secret was applied after cleanup failure; calls: %s", formatCalls(calls))
				}
				if _, statErr := os.Stat(harness.manifestPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("token manifest exists after cleanup failure: %v", statErr)
				}
				return
			}

			if len(applyIndexes) != 1 || deleteIndexes[0] >= applyIndexes[0] {
				t.Fatalf("cleanup must precede the single token apply; calls: %s", formatCalls(calls))
			}
			manifest, readErr := os.ReadFile(harness.manifestPath)
			if readErr != nil {
				t.Fatalf("read token manifest: %v", readErr)
			}
			if !strings.Contains(string(manifest), "kind: Secret") || !strings.Contains(string(manifest), "type: bootstrap.kubernetes.io/token") {
				t.Fatalf("applied manifest is not a bootstrap token Secret:\n%s", manifest)
			}
		})
	}
}

func TestKubeadmRBACReconciliation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		applyFailAt    int
		deleteExitCode int
		wantApplies    int
		wantDeletes    int
		wantFailure    bool
	}{
		{name: "succeeds", wantApplies: 2, wantDeletes: 1},
		{name: "initial RBAC apply fails", applyFailAt: 1, wantApplies: 1, wantFailure: true},
		{name: "legacy cleanup fails", deleteExitCode: fakeDeleteFailure, wantApplies: 1, wantDeletes: 1, wantFailure: true},
		{name: "ConfigMap apply fails", applyFailAt: 2, wantApplies: 2, wantDeletes: 1, wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, true, test.deleteExitCode)
			harness.applyFailAt = test.applyFailAt
			output, err := harness.runKubeadmEnsureRBAC()
			if test.wantFailure && err == nil {
				t.Fatalf("kubeadm RBAC reconciliation succeeded despite injected failure\n%s", output)
			}
			if test.wantFailure {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
					t.Fatalf("kubeadm RBAC reconciliation error = %v, want exit code 1\n%s", err, output)
				}
			} else if err != nil {
				t.Fatalf("kubeadm RBAC reconciliation failed: %v\n%s", err, output)
			}

			calls := readCommandCalls(t, harness.commandLogPath)
			applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
			if len(applyIndexes) != test.wantApplies || len(deleteIndexes) != test.wantDeletes {
				t.Fatalf(
					"kubectl apply/delete counts = %d/%d, want %d/%d; calls: %s",
					len(applyIndexes), len(deleteIndexes), test.wantApplies, test.wantDeletes, formatCalls(calls),
				)
			}
			if len(deleteIndexes) == 1 {
				if applyIndexes[0] >= deleteIndexes[0] {
					t.Errorf("legacy cleanup ran before safe RBAC apply; calls: %s", formatCalls(calls))
				}
				assertLegacyDeleteCall(t, calls[deleteIndexes[0]])
			}
		})
	}
}

func TestRepositoryDoesNotBindBootstrapGroupsToLegacyNodeRole(t *testing.T) {
	t.Parallel()

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repositoryRoot := filepath.Dir(workingDir)
	sourceRoots := []string{"cmd", "hack", "pkg", "scripts"}
	bindingPattern := regexp.MustCompile(`(?m)^[ \t]*kind:[ \t]*ClusterRoleBinding[ \t]*$`)
	legacyRolePattern := regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*system:node[ \t]*$`)
	bootstrapGroupPattern := regexp.MustCompile(`(?m)^[ \t]*name:[ \t]*system:bootstrappers:[^ \t\n]+[ \t]*$`)
	documentSeparator := regexp.MustCompile(`(?m)^[ \t]*---[ \t]*$`)

	for _, sourceRoot := range sourceRoots {
		root := filepath.Join(repositoryRoot, sourceRoot)
		walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			extension := filepath.Ext(path)
			if entry.Name() != "aks-flex-config" && extension != ".go" && extension != ".py" && extension != ".sh" && extension != ".yaml" && extension != ".yml" {
				return nil
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("read %s: %w", path, readErr)
			}
			normalized := strings.ReplaceAll(string(contents), "\r\n", "\n")
			for documentIndex, document := range documentSeparator.Split(normalized, -1) {
				if bindingPattern.MatchString(document) && legacyRolePattern.MatchString(document) && bootstrapGroupPattern.MatchString(document) {
					relativePath, relErr := filepath.Rel(repositoryRoot, path)
					if relErr != nil {
						relativePath = path
					}
					t.Errorf("%s YAML document %d binds a bootstrap group to the broad legacy %q role", relativePath, documentIndex+1, legacyNodeRole)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("scan %s for unsafe bootstrap RBAC: %v", sourceRoot, walkErr)
		}
	}
}

func newConfigScriptHarness(t *testing.T, legacyPresent bool, deleteExitCode int) *configScriptHarness {
	t.Helper()

	pythonPath, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to test scripts/aks-flex-config")
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	scriptPath := filepath.Join(workingDir, "aks-flex-config")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("stat aks-flex-config: %v", err)
	}

	tempDir := t.TempDir()
	fakeBinDir := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBinDir, 0o700); err != nil {
		t.Fatalf("create fake bin directory: %v", err)
	}
	writeExecutable(t, filepath.Join(fakeBinDir, "az"), fakeAZScript)
	writeExecutable(t, filepath.Join(fakeBinDir, "kubectl"), fakeKubectlScript)

	legacyState := filepath.Join(tempDir, "legacy-state")
	state := "absent\n"
	if legacyPresent {
		state = "present\n"
	}
	if err := os.WriteFile(legacyState, []byte(state), 0o600); err != nil {
		t.Fatalf("write fake legacy state: %v", err)
	}

	return &configScriptHarness{
		pythonPath:     pythonPath,
		scriptPath:     scriptPath,
		fakeBinDir:     fakeBinDir,
		commandLogPath: filepath.Join(tempDir, "commands.log"),
		manifestPath:   filepath.Join(tempDir, "rbac.yaml"),
		configPath:     filepath.Join(tempDir, "config.json"),
		legacyState:    legacyState,
		deleteExitCode: deleteExitCode,
		applyCountPath: filepath.Join(tempDir, "apply-count"),
	}
}

func (h *configScriptHarness) runSetupNodeRBAC() (string, error) {
	cmd := exec.Command(
		h.pythonPath,
		h.scriptPath,
		"setup-node-rbac",
		"--resource-group", "test-rg",
		"--cluster-name", "test-cluster",
		"--subscription", "test-subscription",
	)
	cmd.Env = append(os.Environ(),
		"PATH="+h.fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+filepath.Join(filepath.Dir(h.fakeBinDir), "kubeconfig"),
		"PYTHONDONTWRITEBYTECODE=1",
		fakeCommandLogEnv+"="+h.commandLogPath,
		fakeManifestEnv+"="+h.manifestPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fakeApplyCountEnv+"="+h.applyCountPath,
		fmt.Sprintf("%s=%d", fakeApplyFailAtEnv, h.applyFailAt),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (h *configScriptHarness) runGenerateNodeConfig() (string, error) {
	cmd := exec.Command(
		h.pythonPath,
		h.scriptPath,
		"generate-node-config",
		"--resource-group", "test-rg",
		"--cluster-name", "test-cluster",
		"--subscription", "test-subscription",
		"--bootstrap-token",
		"--output", h.configPath,
	)
	cmd.Env = append(os.Environ(),
		"PATH="+h.fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+filepath.Join(filepath.Dir(h.fakeBinDir), "kubeconfig"),
		"PYTHONDONTWRITEBYTECODE=1",
		fakeCommandLogEnv+"="+h.commandLogPath,
		fakeManifestEnv+"="+h.manifestPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fakeApplyCountEnv+"="+h.applyCountPath,
		fmt.Sprintf("%s=%d", fakeApplyFailAtEnv, h.applyFailAt),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func (h *configScriptHarness) runKubeadmEnsureRBAC() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	kubeadmScript := filepath.Join(filepath.Dir(workingDir), "hack", "e2e", "lib", "node-join-kubeadm.sh")
	cmd := exec.Command(
		"bash",
		"-c",
		`source "$1"; with_cluster_lock _kubeadm_ensure_rbac "https://test-cluster.example.test:443" "dGVzdC1jYQ=="`,
		"bash",
		kubeadmScript,
	)
	cmd.Env = append(os.Environ(),
		"PATH="+h.fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"E2E_WORK_DIR="+filepath.Join(filepath.Dir(h.fakeBinDir), "e2e-work"),
		"E2E_KUBERNETES_VERSION=1.35.0",
		fakeCommandLogEnv+"="+h.commandLogPath,
		fakeManifestEnv+"="+h.manifestPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fakeApplyCountEnv+"="+h.applyCountPath,
		fmt.Sprintf("%s=%d", fakeApplyFailAtEnv, h.applyFailAt),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func readRBACManifest(t *testing.T, path string) []rbacv1.ClusterRoleBinding {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read captured RBAC manifest: %v", err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var bindings []rbacv1.ClusterRoleBinding
	for {
		var binding rbacv1.ClusterRoleBinding
		err := decoder.Decode(&binding)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode captured RBAC manifest: %v", err)
		}
		if binding.APIVersion == "" && binding.Kind == "" && binding.Name == "" {
			continue
		}
		bindings = append(bindings, binding)
	}
	return bindings
}

func bindingNames(bindings []rbacv1.ClusterRoleBinding) []string {
	names := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		names = append(names, binding.Name)
	}
	return names
}

func readCommandCalls(t *testing.T, path string) []commandCall {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake command log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	calls := make([]commandCall, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		calls = append(calls, commandCall{name: fields[0], args: fields[1:]})
	}
	return calls
}

func kubectlOperationIndexes(calls []commandCall) (apply []int, delete []int) {
	for i, call := range calls {
		if call.name != "kubectl" || len(call.args) == 0 {
			continue
		}
		switch call.args[0] {
		case "apply":
			apply = append(apply, i)
		case "delete":
			delete = append(delete, i)
		}
	}
	return apply, delete
}

func assertLegacyDeleteCall(t *testing.T, call commandCall) {
	t.Helper()

	if call.name != "kubectl" || len(call.args) < 2 || call.args[0] != "delete" {
		t.Fatalf("migration call = %#v, want kubectl delete", call)
	}
	resourceMatches := false
	for i, arg := range call.args[1:] {
		if arg == "clusterrolebinding/"+legacyBindingName {
			resourceMatches = true
			break
		}
		if (arg == "clusterrolebinding" || arg == "clusterrolebindings") && i+2 < len(call.args) && call.args[i+2] == legacyBindingName {
			resourceMatches = true
			break
		}
	}
	if !resourceMatches {
		t.Errorf("migration delete args = %q, want ClusterRoleBinding %q", call.args, legacyBindingName)
	}
	if !containsIgnoreNotFound(call.args) {
		t.Errorf("migration delete args = %q, want --ignore-not-found for idempotency", call.args)
	}
}

func containsIgnoreNotFound(args []string) bool {
	for _, arg := range args {
		if arg == "--ignore-not-found" || arg == "--ignore-not-found=true" {
			return true
		}
	}
	return false
}

func formatCalls(calls []commandCall) string {
	formatted := make([]string, 0, len(calls))
	for _, call := range calls {
		formatted = append(formatted, strings.Join(append([]string{call.name}, call.args...), " "))
	}
	return strings.Join(formatted, "; ")
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatalf("write fake executable %s: %v", path, err)
	}
}

const fakeAZScript = `#!/bin/sh
set -eu
{
    printf 'az'
    for arg in "$@"; do
        printf '\t%s' "$arg"
    done
    printf '\n'
} >> "${AKS_FLEX_CONFIG_TEST_COMMAND_LOG:?}"

case " $* " in
*" account show "*" --query id "*) printf 'test-subscription\n' ;;
*" account show "*" --query tenantId "*) printf 'test-tenant\n' ;;
*" aks show "*" --query id "*) printf '/subscriptions/test/resourceGroups/test-rg/providers/Microsoft.ContainerService/managedClusters/test-cluster\n' ;;
*" aks show "*" --query location "*) printf 'test-region\n' ;;
*" currentKubernetesVersion "*) printf '1.35.0\n' ;;
*" networkProfile.dnsServiceIp "*) printf '10.0.0.10\n' ;;
esac
`

const fakeKubectlScript = `#!/bin/sh
set -eu
{
    printf 'kubectl'
    for arg in "$@"; do
        printf '\t%s' "$arg"
    done
    printf '\n'
} >> "${AKS_FLEX_CONFIG_TEST_COMMAND_LOG:?}"

case "${1:-}" in
apply)
    apply_count=0
    if [ -f "${AKS_FLEX_CONFIG_TEST_APPLY_COUNT:?}" ]; then
        apply_count=$(cat "$AKS_FLEX_CONFIG_TEST_APPLY_COUNT")
    fi
    apply_count=$((apply_count + 1))
    printf '%s\n' "$apply_count" > "$AKS_FLEX_CONFIG_TEST_APPLY_COUNT"
    if [ "${AKS_FLEX_CONFIG_TEST_APPLY_FAIL_AT:-0}" -eq "$apply_count" ]; then
        exit 38
    fi
    cat > "${AKS_FLEX_CONFIG_TEST_MANIFEST:?}"
    ;;
config)
    case " $* " in
    *"certificate-authority-data"*) printf 'dGVzdC1jYQ==\n' ;;
    *"cluster.server"*) printf 'https://test-cluster.example.test:443\n' ;;
    *) exit 45 ;;
    esac
    ;;
delete)
    delete_exit="${AKS_FLEX_CONFIG_TEST_DELETE_EXIT:-0}"
    if [ "$delete_exit" -ne 0 ]; then
        exit "$delete_exit"
    fi

    case " $* " in
    *" aks-flex-node-role "*|*" clusterrolebinding/aks-flex-node-role "*) ;;
    *) exit 43 ;;
    esac

    state=$(cat "${AKS_FLEX_CONFIG_TEST_LEGACY_STATE:?}")
    if [ "$state" = "present" ]; then
        printf 'absent\n' > "$AKS_FLEX_CONFIG_TEST_LEGACY_STATE"
        exit 0
    fi

    for arg in "$@"; do
        case "$arg" in
        --ignore-not-found|--ignore-not-found=true) exit 0 ;;
        esac
    done
    exit 44
    ;;
*)
    exit 42
    ;;
esac
`
