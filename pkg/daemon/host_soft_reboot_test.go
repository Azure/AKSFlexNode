package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/internal/agentdeployment"
	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func softOperation(name string) *machinav1alpha3.MachineOperation {
	return &machinav1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: machinav1alpha3.MachineOperationSpec{
			MachineRef:    "machine1",
			OperationKind: machinav1alpha3.OperationHostSoftReboot,
		},
	}
}

func issuedSoftOperation(
	name, machine, bootID string,
) *machinav1alpha3.MachineOperation {
	op := softOperation(name)
	op.Status.Phase = machinav1alpha3.OperationPhaseInProgress
	op.Status.Targets = []machinav1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: machine,
			Phase:      machinav1alpha3.OperationPhaseInProgress,
			BootID:     bootID,
		},
	}

	return op
}

func softClient(objects ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(objects...).Build()
}

func getSoftOperation(
	t *testing.T,
	c client.Reader,
	name string,
) *machinav1alpha3.MachineOperation {
	t.Helper()

	var op machinav1alpha3.MachineOperation
	if err := c.Get(t.Context(), client.ObjectKey{Name: name}, &op); err != nil {
		t.Fatal(err)
	}

	return &op
}

const hostSoftRebootRetry = 10 * time.Second

type fakeHostRebooter struct {
	bootID func() (string, error)
	reboot func(context.Context, *slog.Logger) error
}

func (f *fakeHostRebooter) BootID() (string, error) {
	return f.bootID()
}

func (f *fakeHostRebooter) RebootHost(ctx context.Context, log *slog.Logger) error {
	return f.reboot(ctx, log)
}

func newTestSoftHandler(
	t *testing.T,
	c client.Client,
	reader client.Reader,
	host *fakeHostRebooter,
	stopping *atomic.Bool,
) *agentdaemon.HostSoftRebootHandler {
	t.Helper()

	target := flexHostSoftRebootTarget{machineName: "machine1", nodeName: "node1"}
	handler, err := agentdaemon.NewHostSoftRebootHandler(c, reader, discardLogger(), target, host, stopping)
	if err != nil {
		t.Fatal(err)
	}

	return handler
}

func newTestStartupHandler(t *testing.T, c client.Client, bootID func() (string, error)) *agentdaemon.HostSoftRebootHandler {
	t.Helper()

	host := &fakeHostRebooter{
		bootID: bootID,
		reboot: func(context.Context, *slog.Logger) error {
			t.Fatal("startup publication invoked reboot")
			return nil
		},
	}

	return newTestSoftHandler(t, c, c, host, &atomic.Bool{})
}

func operationTargetStatus(status *machinav1alpha3.MachineOperationStatus, ref string) *machinav1alpha3.MachineOperationTargetStatus {
	for i := range status.Targets {
		if status.Targets[i].MachineRef == ref {
			return &status.Targets[i]
		}
	}

	return nil
}

type softHarness struct {
	handlers   *machineOperationHandlers
	host       *fakeHostRebooter
	reconciler gatedMachineOperationReconciler
	calls      int
}

func newSoftHarness(
	t *testing.T,
	c client.Client,
	reader client.Reader,
) *softHarness {
	t.Helper()

	gate := newStartupGate()
	gate.open()
	h := &softHarness{}
	h.host = &fakeHostRebooter{bootID: func() (string, error) { return "boot1", nil }}
	h.handlers = &machineOperationHandlers{
		log:          discardLogger(),
		gate:         gate,
		agentUpgrade: &fakeAgentUpgradeExecutor{},
		softReboot:   newTestSoftHandler(t, c, reader, h.host, &gate.stopping),
	}
	h.host.reboot = func(ctx context.Context, _ *slog.Logger) error {
		h.calls++
		op := getSoftOperation(t, reader, "soft")
		target := operationTargetStatus(&op.Status, "machine1")
		if op.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
			t.Fatalf("reboot before aggregate start: %+v", op.Status)
		}
		if target == nil {
			t.Fatal("reboot before target start")
		}
		if target.Phase != machinav1alpha3.OperationPhaseInProgress {
			t.Fatalf("target phase before reboot = %s", target.Phase)
		}
		if target.BootID != "boot1" {
			t.Fatalf("target boot ID before reboot = %q", target.BootID)
		}

		return nil
	}
	handlers := agentdaemon.MachineOperationHandlers{
		machinav1alpha3.OperationHostSoftReboot: h.handlers.reconcileHostSoftReboot,
	}
	sdk, err := agentdaemon.NewMachinaMachineOperationReconcilerWithReader(c, reader, "machine1", "node1", handlers)
	if err != nil {
		t.Fatal(err)
	}

	h.reconciler = gatedMachineOperationReconciler{
		delegate: &flexMachineOperationReconciler{
			delegate:   sdk,
			handlers:   handlers,
			reader:     reader,
			target:     flexHostSoftRebootTarget{machineName: "machine1", nodeName: "node1"},
			softReboot: h.handlers.softReboot,
		},
		gate: gate,
	}

	return h
}

type trackingActivationLock struct {
	*fakeAgentUpgradeExecutor
	held     bool
	acquired int
	closed   int
}

func (l *trackingActivationLock) Acquire() (io.Closer, error) {
	l.acquired++
	l.held = true
	return l, nil
}

func (l *trackingActivationLock) Close() error {
	l.closed++
	l.held = false
	return nil
}

func TestHostSoftRebootHoldsActivationLock(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		wait       bool
		writeError bool
		commandErr bool
		wantWrites int
		wantCalls  int
		wantErr    bool
	}{
		"out-of-band wait": {
			wait:       true,
			wantWrites: 1,
		},
		"start write error": {
			writeError: true,
			wantWrites: 1,
			wantErr:    true,
		},
		"command error": {
			commandErr: true,
			wantWrites: 2,
			wantCalls:  1,
		},
		"success": {
			wantWrites: 1,
			wantCalls:  1,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			lock := &trackingActivationLock{
				fakeAgentUpgradeExecutor: &fakeAgentUpgradeExecutor{},
			}
			objects := []client.Object{softOperation("soft")}
			if tc.wait {
				blocker := softOperation("oob")
				blocker.Spec.OperationKind = machinav1alpha3.OperationHostHardReboot
				objects = append(objects, blocker)
			}
			lists, writes, calls := 0, 0, 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists++
					if !lock.held || lock.acquired != 1 || lock.closed != 0 {
						t.Error("live OOB list ran without the activation lock")
					}
					return c.List(ctx, list, opts...)
				},
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					if !lock.held || lock.closed != 0 {
						t.Error("status write ran without the activation lock")
					}
					if tc.writeError {
						return errors.New("start write failed")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			h := newSoftHarness(t, c, c)
			h.handlers.agentUpgrade = lock
			h.host.reboot = func(context.Context, *slog.Logger) error {
				calls++
				if !lock.held || lock.closed != 0 {
					t.Error("command ran without the activation lock")
				}
				if tc.commandErr {
					return errors.New("command failed")
				}
				return nil
			}

			result, err := h.handlers.reconcileHostSoftReboot(t.Context(), &fakeMachineOperationStore{}, agentdaemon.MachineOperation{Name: "soft"})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if (result.RequeueAfter == hostSoftRebootRetry) != tc.wait {
				t.Fatalf("requeue = %v, wait = %v", result.RequeueAfter, tc.wait)
			}
			if lists != 1 {
				t.Fatalf("lists = %d, want 1", lists)
			}
			if writes != tc.wantWrites {
				t.Fatalf("writes = %d, want %d", writes, tc.wantWrites)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls, tc.wantCalls)
			}
			if lock.acquired != 1 {
				t.Fatalf("lock acquisitions = %d, want 1", lock.acquired)
			}
			if lock.closed != 1 {
				t.Fatalf("lock closes = %d, want 1", lock.closed)
			}
			if lock.held {
				t.Fatal("activation lock still held")
			}
		})
	}
}

func TestHostSoftRebootActivationLockContention(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	h.handlers.agentUpgrade = &fakeAgentUpgradeExecutor{acquireErr: agentbinary.ErrActivationInProgress}

	result, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeue = %v, want a retry", result.RequeueAfter)
	}
	if h.calls != 0 {
		t.Fatalf("reboot calls = %d, want 0", h.calls)
	}
	if h.handlers.gate.stopping.Load() {
		t.Fatal("lock contention stopped admission")
	}
	if phase := getSoftOperation(t, c, "soft").Status.Phase; phase != "" {
		t.Fatalf("lock contention changed phase to %s", phase)
	}
}

func TestHostSoftRebootUsesDirectReaderForBlockersAndSelectors(t *testing.T) {
	t.Parallel()

	soft := softOperation("soft")
	soft.Spec.MachineRef = ""
	soft.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	oob := softOperation("oob")
	oob.Spec.OperationKind = machinav1alpha3.OperationHostPowerOff
	oob.Spec.MachineRef = ""
	oob.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	apiNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
	cachedNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "b",
			},
		},
	}
	api := softClient(soft, oob, apiNode)
	cached := softClient(soft.DeepCopy(), cachedNode)
	// Use a stale read client with writes forwarded to the API client.
	writer := &staleReadClient{Client: api, stale: cached}
	h := newSoftHarness(t, writer, api)
	result, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft")
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != hostSoftRebootRetry {
		t.Fatalf("requeue = %v", result.RequeueAfter)
	}
	if h.calls != 0 {
		t.Fatalf("reboot calls = %d, want 0", h.calls)
	}
	if got := getSoftOperation(t, api, "soft").Status.Message; !strings.Contains(got, "oob") {
		t.Fatalf("message = %q", got)
	}
}

type staleReadClient struct {
	client.Client
	stale client.Reader
}

func (c *staleReadClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	return c.stale.Get(ctx, key, obj, opts...)
}

func (c *staleReadClient) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	return c.stale.List(ctx, list, opts...)
}

type countRepave struct{ calls int }

func (r *countRepave) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b
}
func (r *countRepave) ReconcileRepave(context.Context, string) (ctrl.Result, error) {
	r.calls++
	return ctrl.Result{}, nil
}

func TestHostSoftRebootDefersRepave(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	r := &countRepave{}
	wrapped := gatedRepaveReconciler{delegate: r, gate: h.handlers.gate}
	if _, err := wrapped.ReconcileRepave(t.Context(), "node-change"); err != nil {
		t.Fatal(err)
	}
	if r.calls != 0 {
		t.Fatal("repave ran during reboot handoff")
	}
}

type countMachineOperations struct{ calls int }

func (r *countMachineOperations) SetupController(b *builder.TypedBuilder[agentdaemon.Request]) *builder.TypedBuilder[agentdaemon.Request] {
	return b
}

func (r *countMachineOperations) ReconcileMachineOperation(context.Context, string) (ctrl.Result, error) {
	r.calls++
	return ctrl.Result{}, nil
}

func TestHostSoftRebootDefersAllAgentOperations(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	delegate := &countMachineOperations{}
	wrapped := gatedMachineOperationReconciler{delegate: delegate, gate: h.handlers.gate}
	for _, name := range []string{"another-host-reboot", "node-reboot", "agent-upgrade", "agent-reset"} {
		if _, err := wrapped.ReconcileMachineOperation(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if delegate.calls != 0 {
		t.Fatalf("dispatched %d operations during shutdown", delegate.calls)
	}
}

func TestAgentUpgradeHandoffDefersHostSoftReboot(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	op := agentdaemon.MachineOperation{
		Name: "upgrade",
		Parameters: map[string]string{
			agentdeployment.UpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
			agentdeployment.UpgradeSHA256Parameter:      strings.Repeat("a", 64),
		},
	}
	if _, err := h.handlers.reconcileAgentUpgrade(t.Context(), &fakeMachineOperationStore{}, op); err != nil {
		t.Fatal(err)
	}
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 0 || getSoftOperation(t, c, "soft").Status.Phase != "" {
		t.Fatal("host reboot started during upgrade handoff")
	}
}

func TestAgentContainerSystemctlUsesHostExec(t *testing.T) {
	t.Parallel()

	dockerfile, err := os.ReadFile("../../Dockerfile.aks-flex-node-agent")
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile("../../scripts/agent-image/host-exec")
	if err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile("../internal/agentdeployment/assets/aks-flex-node-agent-container.service")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"for tool in machinectl systemctl", "ln -s host-exec /opt/aks-flex-node/bin/$tool", "ENV PATH=/opt/aks-flex-node/bin:"} {
		if !strings.Contains(string(dockerfile), text) {
			t.Fatalf("container image missing %q", text)
		}
	}
	if !strings.Contains(string(script), `exec /usr/sbin/chroot "$host_root" /usr/bin/env "$host_path" "$name" "$@"`) {
		t.Fatal("host-exec does not invoke host tools in the host root")
	}
	for _, text := range []string{"--with-ns pid:/proc/1/ns/pid", "--mount type=bind,src=/,dst=/host"} {
		if !strings.Contains(string(unit), text) {
			t.Fatalf("container unit missing %q", text)
		}
	}
}

func TestHostSoftRebootStartupPublicationRetriesBeforeGateOpens(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"write", "boot"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()

			writes := 0
			c := fake.NewClientBuilder().WithScheme(newScheme()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
				WithObjects(issuedSoftOperation("soft", "machine1", "boot1")).WithInterceptorFuncs(interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
					writes++
					if failure == "write" && writes == 1 {
						return errors.New("temporary status failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}).Build()
			gate := newStartupGate()
			attempts := 0
			openGateAfterAgentUpgradeJudgment(t.Context(), discardLogger(), func(ctx context.Context) error {
				attempts++
				select {
				case <-gate.ready:
					t.Error("gate opened before reboot publication")
				default:
				}
				if attempts == 2 {
					op := getSoftOperation(t, c, "soft")
					if op.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
						t.Fatalf("failed publication changed phase: %+v", op.Status)
					}
					if op.Status.Targets[0].BootID != "boot1" {
						t.Fatalf("failed publication changed boot ID: %+v", op.Status.Targets[0])
					}
				}
				return newTestStartupHandler(t, c, func() (string, error) {
					if failure == "boot" && attempts == 1 {
						return "", errors.New("temporary boot read failure")
					}
					return "boot2", nil
				}).PublishStartup(ctx)
			}, func() (*agentdeployment.UpgradeSignal, error) {
				return nil, nil
			}, noHeldAssets, gate.open, time.Millisecond)

			if attempts != 2 {
				t.Fatalf("attempts = %d, want 2", attempts)
			}
			if op := getSoftOperation(t, c, "soft"); op.Status.Phase != machinav1alpha3.OperationPhaseComplete {
				t.Fatalf("status = %+v", op.Status)
			}
			select {
			case <-gate.ready:
			default:
				t.Fatal("gate did not open after publication")
			}
		})
	}
}

func TestHostSoftRebootInitialBootReadFailure(t *testing.T) {
	t.Parallel()

	c := softClient(softOperation("soft"))
	h := newSoftHarness(t, c, c)
	h.host.bootID = func() (string, error) { return "", errors.New("boot failed") }

	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatalf("boot failure publication: %v", err)
	}
	if h.calls != 0 {
		t.Fatalf("reboot calls = %d, want 0", h.calls)
	}
	op := getSoftOperation(t, c, "soft")
	if op.Status.Phase != machinav1alpha3.OperationPhaseFailed {
		t.Fatalf("phase = %s, want Failed", op.Status.Phase)
	}
	condition := apimeta.FindStatusCondition(op.Status.Conditions, "Completed")
	if condition == nil {
		t.Fatal("missing Completed condition")
	}
	if condition.Reason != "ExecutionFailed" {
		t.Fatalf("reason = %s, want ExecutionFailed", condition.Reason)
	}
	if condition.Message != "boot failed" {
		t.Fatalf("message = %q, want boot failed", condition.Message)
	}
}

func TestHostSoftRebootConsumerWiring(t *testing.T) {
	t.Parallel()

	for _, mode := range []isolation.Mode{isolation.ModeNSpawn, isolation.ModeNone, isolation.ModeContainer} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			op := softOperation("soft")
			op.Spec.MachineRef = ""
			op.Spec.MachineSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"rack": "a"},
			}
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "node1",
					Labels: map[string]string{"rack": "a"},
				},
			}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithRESTMapper(machineOperationRESTMapper()).
				WithStatusSubresource(&machinav1alpha3.MachineOperation{}).WithObjects(op, node).Build()
			worker := &fakeWorkerRuntime{}
			lock := &trackingActivationLock{
				fakeAgentUpgradeExecutor: &fakeAgentUpgradeExecutor{},
			}
			gate := newStartupGate()
			gate.open()
			log := discardLogger()
			ctx := t.Context()
			bootID := "boot1"
			calls := 0
			host := &fakeHostRebooter{
				bootID: func() (string, error) { return bootID, nil },
				reboot: func(gotCtx context.Context, gotLog *slog.Logger) error {
					calls++
					if gotCtx != ctx || gotLog != log {
						t.Fatal("host command did not receive the reconcile context and logger")
					}
					if !lock.held || !gate.stopping.Load() {
						t.Fatal("host command ran without activation lock and stopping gate")
					}
					current := getSoftOperation(t, c, "soft")
					target := operationTargetStatus(&current.Status, "machine1")
					if current.Status.Phase != machinav1alpha3.OperationPhaseInProgress {
						t.Fatalf("aggregate not started: %+v", current.Status)
					}
					if target == nil {
						t.Fatal("target missing before host command")
					}
					if target.Phase != machinav1alpha3.OperationPhaseInProgress {
						t.Fatalf("target not started: %+v", target)
					}
					if target.BootID != "boot1" {
						t.Fatalf("target boot evidence = %q", target.BootID)
					}
					if current.Status.ObservedMachineGeneration != 0 || target.ObservedGeneration != 0 {
						t.Fatalf("Flex acquired a Machine generation: %+v", current.Status)
					}

					return nil
				},
			}
			reconciler, err := machineOperationReconciler(machineOperationReconcilerOptions{
				Client:         c,
				Reader:         c,
				Log:            log,
				NodeName:       "node1",
				AKSMachineName: "machine1",
				Runtime:        worker,
				State:          worker,
				Config: &config.Config{
					Bootstrap: config.BootstrapConfig{Isolation: mode},
				},
				Uninstall:    worker.uninstall,
				ClearConfig:  testTask{name: "clear"},
				Resolve:      fakeResolveMachine,
				AgentUpgrade: lock,
				Gate:         gate,
			}, host)
			if err != nil {
				t.Fatal(err)
			}

			flex, ok := reconciler.(*flexMachineOperationReconciler)
			if !ok {
				t.Fatalf("reconciler type = %T", reconciler)
			}
			if len(flex.mapOperation(ctx, op)) != 1 {
				t.Fatal("matching Node selector was not queued")
			}

			wrapped := gatedMachineOperationReconciler{delegate: reconciler, gate: gate}
			if _, err := wrapped.ReconcileMachineOperation(ctx, "soft"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || worker.restarted || lock.closed != 1 || lock.held {
				t.Fatalf("calls = %d, worker restarted = %v, lock closed = %d, held = %v", calls, worker.restarted, lock.closed, lock.held)
			}

			bootID = "boot2"
			if err := flex.softReboot.PublishStartup(ctx); err != nil {
				t.Fatal(err)
			}

			current := getSoftOperation(t, c, "soft")
			if current.Status.Phase != machinav1alpha3.OperationPhaseComplete || current.Status.Targets[0].MachineRef != "machine1" {
				t.Fatalf("startup status = %+v", current.Status)
			}
		})
	}
}

func TestHostSoftRebootExplicitReferenceNeedsNoNodeOrMachine(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().WithScheme(newScheme()).
		WithStatusSubresource(&machinav1alpha3.MachineOperation{}).
		WithObjects(softOperation("soft")).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*machinav1alpha3.MachineOperation); !ok {
				return fmt.Errorf("unexpected lookup of %T", obj)
			}

			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	h := newSoftHarness(t, c, c)
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 {
		t.Fatalf("host reboot calls = %d, want 1", h.calls)
	}
}

func TestFlexHostSoftRebootTarget(t *testing.T) {
	t.Parallel()

	target := flexHostSoftRebootTarget{machineName: "machine1", nodeName: "node1"}
	if target.Reference() != "machine1" {
		t.Fatalf("reference = %q", target.Reference())
	}
	generation, err := target.ObservedGeneration(t.Context(), softClient())
	if err != nil || generation != 0 {
		t.Fatalf("generation = %d, error = %v", generation, err)
	}

	selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{
			{
				Key:      "rack",
				Operator: metav1.LabelSelectorOpIn,
				Values:   []string{"a", "b"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		rack    string
		readErr error
		want    bool
		wantErr bool
	}{
		{
			name: "matching node",
			rack: "a",
			want: true,
		},
		{
			name: "nonmatching node",
			rack: "c",
		},
		{
			name: "missing node",
		},
		{
			name:    "read failure",
			readErr: errors.New("unavailable"),
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			objects := []client.Object{}
			if tc.rack != "" {
				objects = append(objects, &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "node1",
						Labels: map[string]string{"rack": tc.rack},
					},
				})
			}
			c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Node); !ok || key.Name != "node1" {
						return fmt.Errorf("unexpected lookup %T %s", obj, key.Name)
					}
					if tc.readErr != nil {
						return tc.readErr
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()

			matches, err := target.MatchSelector(t.Context(), c, machinav1alpha3.OperationHostPowerOff, selector)
			if matches != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("match = %v, error = %v", matches, err)
			}
			if tc.wantErr && (!errors.Is(err, tc.readErr) || !strings.Contains(err.Error(), "get Node node1")) {
				t.Fatalf("error lacks Node context: %v", err)
			}
		})
	}
}

func TestFlexOperationWatchUsesLiveIdentityAndLabels(t *testing.T) {
	t.Parallel()

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				"rack": "a",
			},
		},
	}
	op := softOperation("soft")
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"rack": "a",
		},
	}
	c := softClient(node, op)
	h := newSoftHarness(t, c, c)
	watch := h.reconciler.delegate.(*flexMachineOperationReconciler)
	if len(watch.mapOperation(t.Context(), op)) != 1 {
		t.Fatal("selector operation was not queued")
	}
	if _, err := h.reconciler.ReconcileMachineOperation(t.Context(), "soft"); err != nil {
		t.Fatal(err)
	}
	if h.calls != 1 {
		t.Fatalf("reboot calls = %d, want 1", h.calls)
	}

	foreign := softOperation("foreign")
	foreign.Spec.MachineRef = "node1"
	if len(watch.mapOperation(t.Context(), foreign)) != 0 {
		t.Fatal("node name admitted as machine identity")
	}

	missingNode := softClient(op.DeepCopy())
	matches, err := watch.target.MatchSelector(t.Context(), missingNode, op.Spec.OperationKind, labels.SelectorFromSet(labels.Set{"rack": "a"}))
	if err != nil || matches {
		t.Fatalf("missing Node: match = %v, error = %v", matches, err)
	}
}
