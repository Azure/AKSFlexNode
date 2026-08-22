package scripts

import (
	"bytes"
	"encoding/json"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	bootstrapGroup              = "system:bootstrappers:aks-flex-node"
	bootstrapBindingName        = "aks-flex-node-bootstrapper"
	bootstrapRole               = "system:node-bootstrapper"
	approvalBindingName         = "aks-flex-node-auto-approve-csr"
	approvalRole                = "system:certificates.k8s.io:certificatesigningrequests:nodeclient"
	legacyBindingName           = "aks-flex-node-role"
	legacyNodeRole              = "system:node"
	fakeDeleteFailure           = 37
	fakeGetFailure              = 39
	fakeCommandLogEnv           = "AKS_FLEX_CONFIG_TEST_COMMAND_LOG"
	fakeManifestEnv             = "AKS_FLEX_CONFIG_TEST_MANIFEST"
	fakeManagedStateEnv         = "AKS_FLEX_CONFIG_TEST_MANAGED_STATE"
	fakeDeleteOptsEnv           = "AKS_FLEX_CONFIG_TEST_DELETE_OPTIONS"
	fakeLegacyStateEnv          = "AKS_FLEX_CONFIG_TEST_LEGACY_STATE"
	fakeDeleteExitEnv           = "AKS_FLEX_CONFIG_TEST_DELETE_EXIT"
	fakeDeleteKeepsEnv          = "AKS_FLEX_CONFIG_TEST_DELETE_KEEPS_STATE"
	fakeConcurrentEnv           = "AKS_FLEX_CONFIG_TEST_CONCURRENT_REPLACE"
	fakeConcurrentManagedEnv    = "AKS_FLEX_CONFIG_TEST_CONCURRENT_MANAGED_REPLACE"
	fakeManagedPostconditionEnv = "AKS_FLEX_CONFIG_TEST_MANAGED_POSTCONDITION"
	fakeSkipManagedMutationEnv  = "AKS_FLEX_CONFIG_TEST_SKIP_MANAGED_MUTATION"
	fakeGetExitEnv              = "AKS_FLEX_CONFIG_TEST_GET_EXIT"
	fakeApplyCountEnv           = "AKS_FLEX_CONFIG_TEST_APPLY_COUNT"
	fakeApplyFailAtEnv          = "AKS_FLEX_CONFIG_TEST_APPLY_FAIL_AT"
)

type commandCall struct {
	name string
	args []string
}

type configScriptHarness struct {
	pythonPath           string
	scriptPath           string
	fakeBinDir           string
	commandLogPath       string
	manifestPath         string
	managedState         string
	deleteOptsPath       string
	configPath           string
	legacyState          string
	deleteExitCode       int
	deleteKeeps          bool
	concurrentSwap       bool
	concurrentManaged    string
	managedPostcondition string
	skipManagedMutation  bool
	getExitCode          int
	applyCountPath       string
	applyFailAt          int
}

func TestSetupNodeRBACManifestUsesOnlyBootstrapPermissions(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	output, err := harness.runSetupNodeRBAC(false)
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

	calls := readCommandCalls(t, harness.commandLogPath)
	applyIndexes, _ := kubectlOperationIndexes(calls)
	if len(applyIndexes) != 2 {
		t.Fatalf("RBAC reconciliation count = %d, want 2; calls: %s", len(applyIndexes), formatCalls(calls))
	}
	for _, index := range applyIndexes {
		assertSafeManagedRBACMutation(t, calls[index])
	}
}

func TestSetupNodeRBACRejectsManagedRoleRefDriftBeforeMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bindingName string
	}{
		{name: "bootstrapper binding", bindingName: bootstrapBindingName},
		{name: "approval binding", bindingName: approvalBindingName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, false, 0)
			bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, true)
			approver := managedBindingFixture(approvalBindingName, approvalRole, true)
			if test.bindingName == bootstrapBindingName {
				bootstrapper.RoleRef.Name = "view"
				bootstrapper.Subjects = append(bootstrapper.Subjects, rbacv1.Subject{
					APIGroup: rbacv1.GroupName,
					Kind:     "Group",
					Name:     "operator-bootstrapper-group",
				})
			} else {
				approver.RoleRef.Name = "view"
				approver.Subjects = append(approver.Subjects, rbacv1.Subject{
					APIGroup: rbacv1.GroupName,
					Kind:     "Group",
					Name:     "operator-approver-group",
				})
			}
			writeManagedState(t, harness.managedState, bootstrapper, approver)
			before := readFile(t, harness.managedState)

			output, err := harness.runSetupNodeRBAC(false)
			if err == nil {
				t.Fatalf("setup-node-rbac replaced a customized roleRef\n%s", output)
			}
			if !strings.Contains(output, test.bindingName) ||
				!strings.Contains(output, "roleRef is immutable") ||
				!strings.Contains(output, "Review it manually") {
				t.Fatalf("failure did not identify the safe manual remediation:\n%s", output)
			}
			if after := readFile(t, harness.managedState); after != before {
				t.Fatalf("managed bindings changed despite preflight failure\nbefore: %s\nafter: %s", before, after)
			}

			calls := readCommandCalls(t, harness.commandLogPath)
			mutations, deletes := kubectlOperationIndexes(calls)
			if len(mutations) != 0 || len(deletes) != 0 {
				t.Fatalf("roleRef preflight performed a mutation: %s", formatCalls(calls))
			}
		})
	}
}

func TestSetupNodeRBACPreservesManagedCustomizations(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, true)
	bootstrapper.Labels = map[string]string{"owner": "operator"}
	bootstrapper.Annotations = map[string]string{"example.test/note": "keep"}
	bootstrapper.Subjects = append(bootstrapper.Subjects, rbacv1.Subject{
		APIGroup: rbacv1.GroupName,
		Kind:     "Group",
		Name:     "operator-group",
	})
	approver := managedBindingFixture(approvalBindingName, approvalRole, true)
	approver.Annotations = map[string]string{"rbac.authorization.kubernetes.io/autoupdate": "false"}
	writeManagedState(t, harness.managedState, bootstrapper, approver)
	before := readFile(t, harness.managedState)

	output, err := harness.runSetupNodeRBAC(false)
	if err != nil {
		t.Fatalf("setup-node-rbac rejected valid customized bindings: %v\n%s", err, output)
	}
	if after := readFile(t, harness.managedState); after != before {
		t.Fatalf("already-correct managed bindings changed\nbefore: %s\nafter: %s", before, after)
	}
	mutations, deletes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath))
	if len(mutations) != 0 || len(deletes) != 0 {
		t.Fatalf("already-correct managed bindings were mutated")
	}
}

func TestSetupNodeRBACAddsSubjectWithOptimisticReplace(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, false)
	bootstrapper.Labels = map[string]string{"owner": "operator"}
	bootstrapper.Subjects = []rbacv1.Subject{{
		APIGroup: rbacv1.GroupName,
		Kind:     "Group",
		Name:     "operator-group",
	}}
	approver := managedBindingFixture(approvalBindingName, approvalRole, true)
	writeManagedState(t, harness.managedState, bootstrapper, approver)

	output, err := harness.runSetupNodeRBAC(false)
	if err != nil {
		t.Fatalf("setup-node-rbac failed to add the required subject: %v\n%s", err, output)
	}
	mutations, deletes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath))
	if len(mutations) != 1 || len(deletes) != 0 {
		t.Fatalf("managed reconciliation mutation/delete counts = %d/%d, want 1/0", len(mutations), len(deletes))
	}
	if call := readCommandCalls(t, harness.commandLogPath)[mutations[0]]; len(call.args) == 0 || call.args[0] != "replace" {
		t.Fatalf("managed binding update = %#v, want optimistic kubectl replace", call)
	}

	updated := findManagedBinding(t, readManagedState(t, harness.managedState), bootstrapBindingName)
	if updated.Labels["owner"] != "operator" {
		t.Fatalf("operator metadata was not preserved: %#v", updated.Labels)
	}
	if !hasSubject(updated.Subjects, "operator-group") || !hasSubject(updated.Subjects, bootstrapGroup) {
		t.Fatalf("operator and required subjects were not both preserved: %#v", updated.Subjects)
	}
	if updated.ResourceVersion != "8" {
		t.Fatalf("resourceVersion = %q, want optimistic replacement of version 7", updated.ResourceVersion)
	}
}

func TestSetupNodeRBACRejectsMissingManagedResourceVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		bindingName string
	}{
		{name: "bootstrapper binding", bindingName: bootstrapBindingName},
		{name: "approval binding", bindingName: approvalBindingName},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, false, 0)
			bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, true)
			approver := managedBindingFixture(approvalBindingName, approvalRole, true)
			if test.bindingName == bootstrapBindingName {
				bootstrapper.ResourceVersion = ""
				bootstrapper.Subjects = nil
			} else {
				approver.ResourceVersion = ""
				approver.Subjects = nil
			}
			writeManagedState(t, harness.managedState, bootstrapper, approver)
			before := readFile(t, harness.managedState)

			output, err := harness.runSetupNodeRBAC(false)
			if err == nil {
				t.Fatalf("setup-node-rbac replaced a binding without a resourceVersion\n%s", output)
			}
			if !strings.Contains(output, test.bindingName) || !strings.Contains(output, "resourceVersion") {
				t.Fatalf("failure did not identify the missing concurrency precondition:\n%s", output)
			}
			if after := readFile(t, harness.managedState); after != before {
				t.Fatalf("managed bindings changed despite a missing resourceVersion\nbefore: %s\nafter: %s", before, after)
			}

			mutations, deletes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath))
			if len(mutations) != 0 || len(deletes) != 0 {
				t.Fatalf("missing resourceVersion preflight performed a mutation")
			}
		})
	}
}

func TestSetupNodeRBACRespectsAutoupdateFalse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		includeBootstrap bool
		wantFailure      bool
	}{
		{name: "missing required subject", wantFailure: true},
		{name: "required subject already present", includeBootstrap: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, false, 0)
			bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, test.includeBootstrap)
			bootstrapper.Annotations = map[string]string{
				"rbac.authorization.kubernetes.io/autoupdate": "false",
			}
			approver := managedBindingFixture(approvalBindingName, approvalRole, true)
			writeManagedState(t, harness.managedState, bootstrapper, approver)

			output, err := harness.runSetupNodeRBAC(false)
			if test.wantFailure && err == nil {
				t.Fatalf("setup-node-rbac changed a protected binding\n%s", output)
			}
			if !test.wantFailure && err != nil {
				t.Fatalf("setup-node-rbac rejected a complete protected binding: %v\n%s", err, output)
			}
			if test.wantFailure && (!strings.Contains(output, "autoupdate") || !strings.Contains(output, "manually")) {
				t.Fatalf("failure did not explain protected-binding remediation:\n%s", output)
			}
			mutations, deletes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath))
			if len(mutations) != 0 || len(deletes) != 0 {
				t.Fatalf("protected binding was mutated")
			}
		})
	}
}

func TestSetupNodeRBACPreservesConcurrentManagedReplacement(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	bootstrapper := managedBindingFixture(bootstrapBindingName, bootstrapRole, false)
	approver := managedBindingFixture(approvalBindingName, approvalRole, true)
	writeManagedState(t, harness.managedState, bootstrapper, approver)
	harness.concurrentManaged = bootstrapBindingName

	output, err := harness.runSetupNodeRBAC(false)
	if err == nil {
		t.Fatalf("setup-node-rbac overwrote a concurrently replaced binding\n%s", output)
	}
	replacement := findManagedBinding(t, readManagedState(t, harness.managedState), bootstrapBindingName)
	if replacement.UID != types.UID("operator-replacement") ||
		replacement.RoleRef.Name != "view" ||
		!hasSubject(replacement.Subjects, "operator-group") {
		t.Fatalf("concurrent operator replacement was not preserved: %#v", replacement)
	}
}

func TestSetupNodeRBACPreservesConcurrentManagedCreate(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	harness.concurrentManaged = bootstrapBindingName

	output, err := harness.runSetupNodeRBAC(false)
	if err == nil {
		t.Fatalf("setup-node-rbac overwrote a concurrently created binding\n%s", output)
	}
	replacement := findManagedBinding(t, readManagedState(t, harness.managedState), bootstrapBindingName)
	if replacement.UID != types.UID("operator-replacement") ||
		replacement.RoleRef.Name != "view" ||
		!hasSubject(replacement.Subjects, "operator-group") {
		t.Fatalf("concurrent operator creation was not preserved: %#v", replacement)
	}

	mutations, deletes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath))
	if len(mutations) != 1 || len(deletes) != 0 {
		t.Fatalf("concurrent create mutation/delete attempts = %d/%d, want 1/0", len(mutations), len(deletes))
	}
}

func TestSetupNodeRBACChecksManagedPostconditions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		configure     func(*configScriptHarness)
		wantSubstring string
	}{
		{
			name: "binding absent",
			configure: func(harness *configScriptHarness) {
				harness.skipManagedMutation = true
			},
			wantSubstring: "absent after reconciliation",
		},
		{
			name: "wrong roleRef",
			configure: func(harness *configScriptHarness) {
				harness.managedPostcondition = bootstrapBindingName + ":wrong-role-ref"
			},
			wantSubstring: "roleRef is immutable",
		},
		{
			name: "required subject missing",
			configure: func(harness *configScriptHarness) {
				harness.managedPostcondition = bootstrapBindingName + ":missing-subject"
			},
			wantSubstring: "does not contain the required bootstrap group",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, false, 0)
			test.configure(harness)
			output, err := harness.runSetupNodeRBAC(false)
			if err == nil {
				t.Fatalf("setup-node-rbac succeeded despite a missing postcondition\n%s", output)
			}
			if !strings.Contains(output, test.wantSubstring) {
				t.Fatalf("postcondition failure was not actionable:\n%s", output)
			}
		})
	}
}

func TestSetupNodeRBACPreservesLegacyBindingWithoutExplicitMigration(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, 0)
	output, err := harness.runSetupNodeRBAC(false)
	if err == nil {
		t.Fatalf("setup-node-rbac succeeded without explicit migration while legacy binding exists\n%s", output)
	}
	if !strings.Contains(output, "--remove-legacy-node-role-binding") ||
		!strings.Contains(output, "v0.1.1") ||
		!strings.Contains(output, "/etc/aks-flex-node/daemon-credentials/daemon-controller-current.pem") {
		t.Fatalf("setup-node-rbac did not explain the compatible migration path:\n%s", output)
	}

	state, readErr := os.ReadFile(harness.legacyState)
	if readErr != nil {
		t.Fatalf("read fake legacy state: %v", readErr)
	}
	if got := strings.TrimSpace(string(state)); got != "present" {
		t.Fatalf("legacy binding state = %q, want present until migration is acknowledged", got)
	}

	calls := readCommandCalls(t, harness.commandLogPath)
	applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
	if len(applyIndexes) != 2 || len(deleteIndexes) != 0 {
		t.Fatalf("kubectl mutation/delete counts = %d/%d, want 2/0; calls: %s", len(applyIndexes), len(deleteIndexes), formatCalls(calls))
	}
	if getIndexes := kubectlIndexes(calls, "get"); len(getIndexes) != 2 || getIndexes[0] >= applyIndexes[0] || applyIndexes[1] >= getIndexes[1] {
		t.Fatalf("safe RBAC must be preflighted and verified before checking legacy migration: %s", formatCalls(calls))
	}
	for _, index := range applyIndexes {
		assertSafeManagedRBACMutation(t, calls[index])
	}
}

func TestSetupNodeRBACMigratesLegacyBindingIdempotently(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, 0)
	for run := 1; run <= 2; run++ {
		output, err := harness.runSetupNodeRBAC(true)
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
	if len(applyIndexes) != 2 || len(deleteIndexes) != 1 {
		t.Fatalf("kubectl apply/delete counts = %d/%d, want 2/1; calls: %s", len(applyIndexes), len(deleteIndexes), formatCalls(calls))
	}
	if applyIndexes[0] >= deleteIndexes[0] {
		t.Errorf("legacy binding was deleted before safe RBAC was applied: %s", formatCalls(calls))
	}
	assertLegacyDeleteCall(t, calls[deleteIndexes[0]])
	assertLegacyDeletePreconditions(t, harness.deleteOptsPath, "legacy-uid", "7")
	if getIndexes := kubectlIndexes(calls, "get"); len(getIndexes) != 5 || getIndexes[1] >= deleteIndexes[0] || deleteIndexes[0] >= getIndexes[2] {
		t.Fatalf("migration must inventory before and verify after deletion, then stay idempotent: %s", formatCalls(calls))
	}
}

func TestSetupNodeRBACRejectsConcurrentLegacyBindingReplacement(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, 0)
	harness.concurrentSwap = true
	output, err := harness.runSetupNodeRBAC(true)
	if err == nil {
		t.Fatalf("setup-node-rbac deleted a concurrently replaced binding\n%s", output)
	}

	state, readErr := os.ReadFile(harness.legacyState)
	if readErr != nil {
		t.Fatalf("read fake legacy state: %v", readErr)
	}
	if got := strings.TrimSpace(string(state)); got != "customized" {
		t.Fatalf("legacy binding state = %q, want concurrently replaced object preserved", got)
	}
	assertLegacyDeletePreconditions(t, harness.deleteOptsPath, "legacy-uid", "7")
}

func TestSetupNodeRBACFailsWhenLegacyBindingDeleteFails(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, fakeDeleteFailure)
	output, err := harness.runSetupNodeRBAC(true)
	if err == nil {
		t.Fatalf("setup-node-rbac succeeded when legacy binding deletion failed\n%s", output)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("setup-node-rbac error = %T %v, want *exec.ExitError", err, err)
	}
	if got := exitErr.ExitCode(); got == 0 {
		t.Fatalf("setup-node-rbac exit code = %d, want nonzero\n%s", got, output)
	}
	if !strings.Contains(output, "HTTP 500") {
		t.Fatalf("setup-node-rbac did not report the API deletion failure:\n%s", output)
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
	if len(applyIndexes) != 2 || len(deleteIndexes) != 1 {
		t.Fatalf("kubectl mutation/delete counts = %d/%d, want 2/1; calls: %s", len(applyIndexes), len(deleteIndexes), formatCalls(calls))
	}
	if applyIndexes[0] >= deleteIndexes[0] {
		t.Errorf("legacy delete failure occurred before safe RBAC was applied; calls: %s", formatCalls(calls))
	}
	assertLegacyDeleteCall(t, calls[deleteIndexes[0]])
}

func TestSetupNodeRBACFailsWhenLegacyBindingRemainsAfterDelete(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, true, 0)
	harness.deleteKeeps = true
	output, err := harness.runSetupNodeRBAC(true)
	if err == nil {
		t.Fatalf("setup-node-rbac succeeded while unsafe binding remained\n%s", output)
	}
	if !strings.Contains(output, "remains after deletion") {
		t.Fatalf("failure did not report the failed postcondition:\n%s", output)
	}

	calls := readCommandCalls(t, harness.commandLogPath)
	_, deleteIndexes := kubectlOperationIndexes(calls)
	if len(deleteIndexes) != 1 || len(kubectlIndexes(calls, "get")) != 3 {
		t.Fatalf("migration did not inventory, delete, and verify: %s", formatCalls(calls))
	}
}

func TestSetupNodeRBACRefusesAmbiguousUnsafeBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state string
	}{
		{name: "canonical name with additional subject", state: "customized"},
		{name: "unexpected binding name", state: "renamed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, false, 0)
			if err := os.WriteFile(harness.legacyState, []byte(test.state+"\n"), 0o600); err != nil {
				t.Fatalf("write fake legacy state: %v", err)
			}
			output, err := harness.runSetupNodeRBAC(true)
			if err == nil {
				t.Fatalf("setup-node-rbac removed an ambiguous binding\n%s", output)
			}
			if !strings.Contains(output, "refusing automatic removal") || !strings.Contains(output, "manually") {
				t.Fatalf("failure did not explain manual remediation:\n%s", output)
			}

			calls := readCommandCalls(t, harness.commandLogPath)
			_, deleteIndexes := kubectlOperationIndexes(calls)
			if len(deleteIndexes) != 0 {
				t.Fatalf("ambiguous binding was deleted: %s", formatCalls(calls))
			}
		})
	}
}

func TestSetupNodeRBACPreservesSameNamedNonLegacyBinding(t *testing.T) {
	t.Parallel()

	harness := newConfigScriptHarness(t, false, 0)
	if err := os.WriteFile(harness.legacyState, []byte("safe-customized\n"), 0o600); err != nil {
		t.Fatalf("write fake legacy state: %v", err)
	}
	output, err := harness.runSetupNodeRBAC(true)
	if err != nil {
		t.Fatalf("setup-node-rbac rejected a non-legacy same-named binding: %v\n%s", err, output)
	}

	state, readErr := os.ReadFile(harness.legacyState)
	if readErr != nil {
		t.Fatalf("read fake legacy state: %v", readErr)
	}
	if got := strings.TrimSpace(string(state)); got != "safe-customized" {
		t.Fatalf("same-named non-legacy binding state = %q, want preserved", got)
	}
	if _, deleteIndexes := kubectlOperationIndexes(readCommandCalls(t, harness.commandLogPath)); len(deleteIndexes) != 0 {
		t.Fatal("same-named non-legacy binding was deleted")
	}
}

func TestGenerateBootstrapTokenRequiresCompletedLegacyMigration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		legacyPresent bool
		getExitCode   int
		wantExitCode  int
	}{
		{name: "migration already complete"},
		{name: "legacy binding present", legacyPresent: true, wantExitCode: 1},
		{name: "legacy state cannot be read", getExitCode: fakeGetFailure, wantExitCode: fakeGetFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, test.legacyPresent, 0)
			harness.getExitCode = test.getExitCode
			output, err := harness.runGenerateNodeConfig()
			if test.wantExitCode != 0 {
				if err == nil {
					t.Fatalf("generate-node-config succeeded before legacy migration completed\n%s", output)
				}
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.wantExitCode {
					t.Fatalf("generate-node-config error = %v, want exit code %d\n%s", err, test.wantExitCode, output)
				}
			} else if err != nil {
				t.Fatalf("generate-node-config failed: %v\n%s", err, output)
			}

			calls := readCommandCalls(t, harness.commandLogPath)
			applyIndexes, deleteIndexes := kubectlOperationIndexes(calls)
			getIndexes := kubectlIndexes(calls, "get")
			if len(getIndexes) != 1 || len(deleteIndexes) != 0 {
				t.Fatalf("kubectl get/delete counts = %d/%d, want 1/0; calls: %s", len(getIndexes), len(deleteIndexes), formatCalls(calls))
			}
			if test.wantExitCode != 0 {
				if len(applyIndexes) != 0 {
					t.Fatalf("token Secret was applied before legacy migration completed; calls: %s", formatCalls(calls))
				}
				if _, statErr := os.Stat(harness.manifestPath); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("token manifest exists after cleanup failure: %v", statErr)
				}
				if test.legacyPresent && !strings.Contains(output, "--remove-legacy-node-role-binding") {
					t.Fatalf("failure did not explain explicit migration path:\n%s", output)
				}
				return
			}

			if len(applyIndexes) != 1 || getIndexes[0] >= applyIndexes[0] {
				t.Fatalf("legacy-state check must precede the single token apply; calls: %s", formatCalls(calls))
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
		name        string
		applyFailAt int
		wantApplies int
		wantFailure bool
	}{
		{name: "succeeds", wantApplies: 2},
		{name: "initial RBAC apply fails", applyFailAt: 1, wantApplies: 1, wantFailure: true},
		{name: "ConfigMap apply fails", applyFailAt: 2, wantApplies: 2, wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			harness := newConfigScriptHarness(t, true, 0)
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
			if len(applyIndexes) != test.wantApplies || len(deleteIndexes) != 0 {
				t.Fatalf(
					"kubectl apply/delete counts = %d/%d, want %d/%d; calls: %s",
					len(applyIndexes), len(deleteIndexes), test.wantApplies, 0, formatCalls(calls),
				)
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
	managedState := filepath.Join(tempDir, "managed-state.json")
	if err := os.WriteFile(managedState, []byte("[]\n"), 0o600); err != nil {
		t.Fatalf("write fake managed binding state: %v", err)
	}

	return &configScriptHarness{
		pythonPath:     pythonPath,
		scriptPath:     scriptPath,
		fakeBinDir:     fakeBinDir,
		commandLogPath: filepath.Join(tempDir, "commands.log"),
		manifestPath:   filepath.Join(tempDir, "rbac.yaml"),
		managedState:   managedState,
		deleteOptsPath: filepath.Join(tempDir, "delete-options.json"),
		configPath:     filepath.Join(tempDir, "config.json"),
		legacyState:    legacyState,
		deleteExitCode: deleteExitCode,
		applyCountPath: filepath.Join(tempDir, "apply-count"),
	}
}

func (h *configScriptHarness) runSetupNodeRBAC(removeLegacy bool) (string, error) {
	args := []string{
		h.scriptPath,
		"setup-node-rbac",
		"--resource-group", "test-rg",
		"--cluster-name", "test-cluster",
		"--subscription", "test-subscription",
	}
	if removeLegacy {
		args = append(args, "--remove-legacy-node-role-binding")
	}
	cmd := exec.Command(h.pythonPath, args...)
	cmd.Env = append(os.Environ(),
		"PATH="+h.fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"KUBECONFIG="+filepath.Join(filepath.Dir(h.fakeBinDir), "kubeconfig"),
		"PYTHONDONTWRITEBYTECODE=1",
		fakeCommandLogEnv+"="+h.commandLogPath,
		fakeManifestEnv+"="+h.manifestPath,
		fakeManagedStateEnv+"="+h.managedState,
		fakeDeleteOptsEnv+"="+h.deleteOptsPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fmt.Sprintf("%s=%t", fakeDeleteKeepsEnv, h.deleteKeeps),
		fmt.Sprintf("%s=%t", fakeConcurrentEnv, h.concurrentSwap),
		fakeConcurrentManagedEnv+"="+h.concurrentManaged,
		fakeManagedPostconditionEnv+"="+h.managedPostcondition,
		fmt.Sprintf("%s=%t", fakeSkipManagedMutationEnv, h.skipManagedMutation),
		fmt.Sprintf("%s=%d", fakeGetExitEnv, h.getExitCode),
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
		fakeManagedStateEnv+"="+h.managedState,
		fakeDeleteOptsEnv+"="+h.deleteOptsPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fmt.Sprintf("%s=%t", fakeDeleteKeepsEnv, h.deleteKeeps),
		fmt.Sprintf("%s=%t", fakeConcurrentEnv, h.concurrentSwap),
		fakeConcurrentManagedEnv+"="+h.concurrentManaged,
		fmt.Sprintf("%s=%t", fakeSkipManagedMutationEnv, h.skipManagedMutation),
		fmt.Sprintf("%s=%d", fakeGetExitEnv, h.getExitCode),
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
		fakeManagedStateEnv+"="+h.managedState,
		fakeDeleteOptsEnv+"="+h.deleteOptsPath,
		fakeLegacyStateEnv+"="+h.legacyState,
		fmt.Sprintf("%s=%d", fakeDeleteExitEnv, h.deleteExitCode),
		fmt.Sprintf("%s=%t", fakeDeleteKeepsEnv, h.deleteKeeps),
		fmt.Sprintf("%s=%t", fakeConcurrentEnv, h.concurrentSwap),
		fakeConcurrentManagedEnv+"="+h.concurrentManaged,
		fmt.Sprintf("%s=%t", fakeSkipManagedMutationEnv, h.skipManagedMutation),
		fmt.Sprintf("%s=%d", fakeGetExitEnv, h.getExitCode),
		fakeApplyCountEnv+"="+h.applyCountPath,
		fmt.Sprintf("%s=%d", fakeApplyFailAtEnv, h.applyFailAt),
	)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func managedBindingFixture(name, role string, includeBootstrap bool) rbacv1.ClusterRoleBinding {
	subjects := []rbacv1.Subject{}
	if includeBootstrap {
		subjects = append(subjects, rbacv1.Subject{
			APIGroup: rbacv1.GroupName,
			Kind:     "Group",
			Name:     bootstrapGroup,
		})
	}
	return rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{
			APIVersion: rbacv1.SchemeGroupVersion.String(),
			Kind:       "ClusterRoleBinding",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			UID:             types.UID(name + "-uid"),
			ResourceVersion: "7",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     role,
		},
		Subjects: subjects,
	}
}

func writeManagedState(t *testing.T, path string, bindings ...rbacv1.ClusterRoleBinding) {
	t.Helper()
	data, err := json.Marshal(bindings)
	if err != nil {
		t.Fatalf("marshal managed binding state: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatalf("write managed binding state: %v", err)
	}
}

func readManagedState(t *testing.T, path string) []rbacv1.ClusterRoleBinding {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read managed binding state: %v", err)
	}
	var bindings []rbacv1.ClusterRoleBinding
	if err := json.Unmarshal(data, &bindings); err != nil {
		t.Fatalf("decode managed binding state: %v", err)
	}
	return bindings
}

func findManagedBinding(t *testing.T, bindings []rbacv1.ClusterRoleBinding, name string) rbacv1.ClusterRoleBinding {
	t.Helper()
	for _, binding := range bindings {
		if binding.Name == name {
			return binding
		}
	}
	t.Fatalf("managed binding %q not found", name)
	return rbacv1.ClusterRoleBinding{}
}

func hasSubject(subjects []rbacv1.Subject, name string) bool {
	for _, subject := range subjects {
		if subject.APIGroup == rbacv1.GroupName && subject.Kind == "Group" && subject.Name == name {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
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
		if call.name == "http-delete" {
			delete = append(delete, i)
			continue
		}
		if call.name != "kubectl" || len(call.args) == 0 {
			continue
		}
		switch call.args[0] {
		case "apply", "create", "replace":
			apply = append(apply, i)
		case "auth":
			if len(call.args) > 1 && call.args[1] == "reconcile" {
				apply = append(apply, i)
			}
		case "delete":
			delete = append(delete, i)
		}
	}
	return apply, delete
}

func kubectlIndexes(calls []commandCall, operation string) []int {
	var indexes []int
	for i, call := range calls {
		if call.name == "kubectl" && len(call.args) > 0 && call.args[0] == operation {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

func assertLegacyDeleteCall(t *testing.T, call commandCall) {
	t.Helper()

	if call.name != "http-delete" || len(call.args) != 1 {
		t.Fatalf("migration call = %#v, want conditional Kubernetes HTTP DELETE", call)
	}
	wantPath := "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/" + legacyBindingName
	if call.args[0] != wantPath {
		t.Errorf("migration delete path = %q, want %q", call.args[0], wantPath)
	}
}

func slicesContain(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func assertLegacyDeletePreconditions(t *testing.T, path, wantUID, wantResourceVersion string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read delete options: %v", err)
	}
	var options struct {
		APIVersion    string `json:"apiVersion"`
		Kind          string `json:"kind"`
		Preconditions struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"preconditions"`
	}
	if err := json.Unmarshal(data, &options); err != nil {
		t.Fatalf("decode delete options: %v", err)
	}
	if options.APIVersion != "v1" || options.Kind != "DeleteOptions" ||
		options.Preconditions.UID != wantUID || options.Preconditions.ResourceVersion != wantResourceVersion {
		t.Fatalf("delete options = %#v, want UID %q and resourceVersion %q", options, wantUID, wantResourceVersion)
	}
}

func assertSafeManagedRBACMutation(t *testing.T, call commandCall) {
	t.Helper()
	if call.name != "kubectl" || len(call.args) != 3 || (call.args[0] != "create" && call.args[0] != "replace") {
		t.Fatalf("RBAC mutation = %#v, want kubectl create/replace -f -", call)
	}
	if !slicesContain(call.args, "-f") || !slicesContain(call.args, "-") || slicesContain(call.args, "--force") {
		t.Errorf("RBAC mutation must be non-destructive and read stdin: %q", call.args)
	}
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
proxy)
    port=""
    for arg in "$@"; do
        case "$arg" in
        --port=*) port="${arg#--port=}" ;;
        esac
    done
    if [ -z "$port" ]; then
        exit 50
    fi
    exec python3 -u - "$port" "${AKS_FLEX_CONFIG_TEST_COMMAND_LOG:?}" "${AKS_FLEX_CONFIG_TEST_DELETE_OPTIONS:?}" "${AKS_FLEX_CONFIG_TEST_LEGACY_STATE:?}" "${AKS_FLEX_CONFIG_TEST_DELETE_EXIT:-0}" "${AKS_FLEX_CONFIG_TEST_DELETE_KEEPS_STATE:-false}" "${AKS_FLEX_CONFIG_TEST_CONCURRENT_REPLACE:-false}" <<'PY'
import http.server
import json
import sys

port, log_path, options_path, state_path, delete_exit, keep_state, concurrent = sys.argv[1:]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_DELETE(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        with open(log_path, "a", encoding="utf-8") as stream:
            stream.write("http-delete\t" + self.path + "\n")
        with open(options_path, "wb") as stream:
            stream.write(body)

        if int(delete_exit) != 0:
            self.respond(500, {"message": "injected delete failure"})
            return
        if concurrent == "true":
            with open(state_path, "w", encoding="utf-8") as stream:
                stream.write("customized\n")
            self.respond(409, {"message": "object changed"})
            return
        if self.path != "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/aks-flex-node-role":
            self.respond(404, {"message": "unexpected resource"})
            return

        try:
            options = json.loads(body)
        except json.JSONDecodeError:
            self.respond(400, {"message": "missing DeleteOptions"})
            return
        preconditions = options.get("preconditions", {})
        with open(state_path, encoding="utf-8") as stream:
            state = stream.read().strip()
        if state != "present":
            self.respond(404, {"message": "not found"})
            return
        if preconditions != {"uid": "legacy-uid", "resourceVersion": "7"}:
            self.respond(409, {"message": "precondition failed"})
            return
        if keep_state != "true":
            with open(state_path, "w", encoding="utf-8") as stream:
                stream.write("absent\n")
        self.respond(200, {"status": "Success"})

    def respond(self, status, payload):
        encoded = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def log_message(self, *_):
        pass

http.server.ThreadingHTTPServer(("127.0.0.1", int(port)), Handler).serve_forever()
PY
    ;;
create|replace)
    apply_count=0
    if [ -f "${AKS_FLEX_CONFIG_TEST_APPLY_COUNT:?}" ]; then
        apply_count=$(cat "$AKS_FLEX_CONFIG_TEST_APPLY_COUNT")
    fi
    apply_count=$((apply_count + 1))
    printf '%s\n' "$apply_count" > "$AKS_FLEX_CONFIG_TEST_APPLY_COUNT"
    if [ "${AKS_FLEX_CONFIG_TEST_APPLY_FAIL_AT:-0}" -eq "$apply_count" ]; then
        exit 38
    fi

    input="${AKS_FLEX_CONFIG_TEST_MANIFEST:?}.input.$$"
    cat > "$input"
    if [ -s "${AKS_FLEX_CONFIG_TEST_MANIFEST}" ]; then
        printf '%s\n' '---' >> "${AKS_FLEX_CONFIG_TEST_MANIFEST}"
    fi
    cat "$input" >> "${AKS_FLEX_CONFIG_TEST_MANIFEST}"
    printf '\n' >> "${AKS_FLEX_CONFIG_TEST_MANIFEST}"

    status=0
    python3 - "$1" "$input" "${AKS_FLEX_CONFIG_TEST_MANAGED_STATE:?}" "${AKS_FLEX_CONFIG_TEST_CONCURRENT_MANAGED_REPLACE:-}" "${AKS_FLEX_CONFIG_TEST_SKIP_MANAGED_MUTATION:-false}" "${AKS_FLEX_CONFIG_TEST_MANAGED_POSTCONDITION:-}" <<'PY' || status=$?
import json
import sys

operation, input_path, state_path, concurrent_name, skip_mutation, postcondition = sys.argv[1:]
with open(input_path, encoding="utf-8") as stream:
    incoming = json.load(stream)
with open(state_path, encoding="utf-8") as stream:
    items = json.load(stream)

name = incoming.get("metadata", {}).get("name")
matches = [index for index, item in enumerate(items) if item.get("metadata", {}).get("name") == name]
if operation == "create":
    if matches:
        raise SystemExit(48)
    if concurrent_name == name:
        replacement = {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRoleBinding",
            "metadata": {
                "name": name,
                "resourceVersion": "concurrent",
                "uid": "operator-replacement",
                "labels": {"owner": "operator"},
            },
            "roleRef": {
                "apiGroup": "rbac.authorization.k8s.io",
                "kind": "ClusterRole",
                "name": "view",
            },
            "subjects": [
                {
                    "apiGroup": "rbac.authorization.k8s.io",
                    "kind": "Group",
                    "name": "operator-group",
                }
            ],
        }
        items.append(replacement)
        with open(state_path, "w", encoding="utf-8") as stream:
            json.dump(items, stream)
        raise SystemExit(48)
    incoming.setdefault("metadata", {})["resourceVersion"] = "1"
    incoming["metadata"]["uid"] = f"{name}-uid"
    if skip_mutation != "true":
        items.append(incoming)
elif operation == "replace":
    if len(matches) != 1:
        raise SystemExit(49)
    index = matches[0]
    current = items[index]
    if concurrent_name == name:
        replacement = {
            "apiVersion": "rbac.authorization.k8s.io/v1",
            "kind": "ClusterRoleBinding",
            "metadata": {
                "name": name,
                "resourceVersion": "concurrent",
                "uid": "operator-replacement",
                "labels": {"owner": "operator"},
            },
            "roleRef": {
                "apiGroup": "rbac.authorization.k8s.io",
                "kind": "ClusterRole",
                "name": "view",
            },
            "subjects": [
                {
                    "apiGroup": "rbac.authorization.k8s.io",
                    "kind": "Group",
                    "name": "operator-group",
                }
            ],
        }
        items[index] = replacement
        with open(state_path, "w", encoding="utf-8") as stream:
            json.dump(items, stream)
        raise SystemExit(41)
    if incoming.get("metadata", {}).get("resourceVersion") != current.get("metadata", {}).get("resourceVersion"):
        raise SystemExit(41)
    incoming["metadata"]["resourceVersion"] = str(int(current["metadata"]["resourceVersion"]) + 1)
    if skip_mutation != "true":
        items[index] = incoming
else:
    raise SystemExit(47)

if skip_mutation != "true" and postcondition:
    postcondition_name, separator, mutation = postcondition.partition(":")
    if not separator:
        raise SystemExit(50)
    if postcondition_name == name:
        target = next(item for item in items if item.get("metadata", {}).get("name") == name)
        if mutation == "wrong-role-ref":
            target["roleRef"]["name"] = "view"
        elif mutation == "missing-subject":
            target["subjects"] = [
                subject for subject in target.get("subjects", [])
                if subject.get("name") != "system:bootstrappers:aks-flex-node"
            ]
        else:
            raise SystemExit(51)

with open(state_path, "w", encoding="utf-8") as stream:
    json.dump(items, stream)
PY
    rm -f "$input"
    exit "$status"
    ;;
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
get)
    get_exit="${AKS_FLEX_CONFIG_TEST_GET_EXIT:-0}"
    if [ "$get_exit" -ne 0 ]; then
        exit "$get_exit"
    fi

    case " $* " in
    *" clusterrolebindings "*) ;;
    *) exit 46 ;;
    esac

    python3 - "${AKS_FLEX_CONFIG_TEST_MANAGED_STATE:?}" "${AKS_FLEX_CONFIG_TEST_LEGACY_STATE:?}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as stream:
    items = json.load(stream)
with open(sys.argv[2], encoding="utf-8") as stream:
    state = stream.read().strip()

subject = {
    "apiGroup": "rbac.authorization.k8s.io",
    "kind": "Group",
    "name": "system:bootstrappers:aks-flex-node",
}
if state == "present":
    items.append({
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "ClusterRoleBinding",
        "metadata": {"name": "aks-flex-node-role", "uid": "legacy-uid", "resourceVersion": "7"},
        "roleRef": {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "ClusterRole",
            "name": "system:node",
        },
        "subjects": [subject],
    })
elif state == "customized":
    items.append({
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "ClusterRoleBinding",
        "metadata": {"name": "aks-flex-node-role", "uid": "replacement-uid", "resourceVersion": "8"},
        "roleRef": {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "ClusterRole",
            "name": "system:node",
        },
        "subjects": [subject, {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "Group",
            "name": "another-group",
        }],
    })
elif state == "renamed":
    items.append({
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "ClusterRoleBinding",
        "metadata": {"name": "custom-bootstrap-node-role", "uid": "renamed-uid", "resourceVersion": "9"},
        "roleRef": {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "ClusterRole",
            "name": "system:node",
        },
        "subjects": [subject],
    })
elif state == "safe-customized":
    items.append({
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "ClusterRoleBinding",
        "metadata": {"name": "aks-flex-node-role"},
        "roleRef": {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "ClusterRole",
            "name": "view",
        },
        "subjects": [{
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "Group",
            "name": "readers",
        }],
    })

print(json.dumps({"items": items}))
PY
    ;;
config)
    case " $* " in
    *"certificate-authority-data"*) printf 'dGVzdC1jYQ==\n' ;;
    *"cluster.server"*) printf 'https://test-cluster.example.test:443\n' ;;
    *) exit 45 ;;
    esac
    ;;
*)
    exit 42
    ;;
esac
`
