package v20260301

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/coreos/go-systemd/v22/dbus"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/kubeadm"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
)

// mockManager is a test double for systemd.Manager. Each method delegates to
// a corresponding function field. Unset fields return nil (success).
type mockManager struct {
	daemonReloadFn     func(ctx context.Context) error
	enableUnitFn       func(ctx context.Context, name string) error
	disableUnitFn      func(ctx context.Context, name string) error
	maskUnitFn         func(ctx context.Context, name string) error
	startUnitFn        func(ctx context.Context, name string) error
	stopUnitFn         func(ctx context.Context, name string) error
	reloadOrRestartFn  func(ctx context.Context, name string) error
	getUnitStatusFn    func(ctx context.Context, name string) (dbus.UnitStatus, error)
	ensureUnitFileFn   func(ctx context.Context, name string, content []byte) (bool, error)
	ensureDropInFileFn func(ctx context.Context, unit, drop string, content []byte) (bool, error)
}

var _ systemd.Manager = (*mockManager)(nil)

func (m *mockManager) DaemonReload(ctx context.Context) error {
	if m.daemonReloadFn != nil {
		return m.daemonReloadFn(ctx)
	}
	return nil
}

func (m *mockManager) EnableUnit(ctx context.Context, name string) error {
	if m.enableUnitFn != nil {
		return m.enableUnitFn(ctx, name)
	}
	return nil
}

func (m *mockManager) DisableUnit(ctx context.Context, name string) error {
	if m.disableUnitFn != nil {
		return m.disableUnitFn(ctx, name)
	}
	return nil
}

func (m *mockManager) MaskUnit(ctx context.Context, name string) error {
	if m.maskUnitFn != nil {
		return m.maskUnitFn(ctx, name)
	}
	return nil
}

func (m *mockManager) StartUnit(ctx context.Context, name string) error {
	if m.startUnitFn != nil {
		return m.startUnitFn(ctx, name)
	}
	return nil
}

func (m *mockManager) StopUnit(ctx context.Context, name string) error {
	if m.stopUnitFn != nil {
		return m.stopUnitFn(ctx, name)
	}
	return nil
}

func (m *mockManager) ReloadOrRestartUnit(ctx context.Context, name string) error {
	if m.reloadOrRestartFn != nil {
		return m.reloadOrRestartFn(ctx, name)
	}
	return nil
}

func (m *mockManager) GetUnitStatus(ctx context.Context, name string) (dbus.UnitStatus, error) {
	if m.getUnitStatusFn != nil {
		return m.getUnitStatusFn(ctx, name)
	}
	return dbus.UnitStatus{}, nil
}

func (m *mockManager) EnsureUnitFile(ctx context.Context, name string, content []byte) (bool, error) {
	if m.ensureUnitFileFn != nil {
		return m.ensureUnitFileFn(ctx, name, content)
	}
	return false, nil
}

func (m *mockManager) EnsureDropInFile(ctx context.Context, unit, drop string, content []byte) (bool, error) {
	if m.ensureDropInFileFn != nil {
		return m.ensureDropInFileFn(ctx, unit, drop, content)
	}
	return false, nil
}

// newFakeKubeadm creates a shell script in dir that exits with the given code.
func newFakeKubeadm(t *testing.T, dir string, exitCode int) string {
	t.Helper()

	script := filepath.Join(dir, "kubeadm")
	content := []byte("#!/bin/sh\nexit " + itoa(exitCode) + "\n")
	if err := os.WriteFile(script, content, 0o755); err != nil { //nolint:gosec // executable script for testing
		t.Fatalf("write fake kubeadm: %v", err)
	}

	return script
}

// itoa converts a small int to its string representation without importing strconv.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}

// buildRequest packs a KubeadmNodeReset into an ApplyActionRequest.
func buildRequest(t *testing.T) *actions.ApplyActionRequest {
	t.Helper()

	msg := kubeadm.KubeadmNodeReset_builder{
		Spec: kubeadm.KubeadmNodeResetSpec_builder{}.Build(),
	}.Build()

	item, err := anypb.New(msg)
	if err != nil {
		t.Fatalf("pack KubeadmNodeReset: %v", err)
	}

	return actions.ApplyActionRequest_builder{Item: item}.Build()
}

func TestNodeResetAction_Success(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeKubeadm := newFakeKubeadm(t, dir, 0)

	// Unit exists and is active → EnsureUnitMasked will stop, disable, mask.
	masked := false
	a := &nodeResetAction{
		systemd: &mockManager{
			getUnitStatusFn: func(_ context.Context, _ string) (dbus.UnitStatus, error) {
				return dbus.UnitStatus{ActiveState: systemd.UnitActiveStateActive}, nil
			},
			stopUnitFn: func(_ context.Context, _ string) error {
				return nil
			},
			disableUnitFn: func(_ context.Context, _ string) error {
				return nil
			},
			maskUnitFn: func(_ context.Context, _ string) error {
				masked = true
				return nil
			},
		},
		kubeadmCommand: fakeKubeadm,
	}

	resp, err := a.ApplyAction(context.Background(), buildRequest(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.GetItem() == nil {
		t.Fatal("expected non-nil response with item")
	}
	if !masked {
		t.Error("expected kubelet unit to be masked")
	}
}

func TestNodeResetAction_KubeadmNotFound(t *testing.T) {
	t.Parallel()

	// Point to a non-existent binary; kubeadmCommand is empty so it falls
	// through to exec.LookPath which won't find "kubeadm" in PATH.
	a := &nodeResetAction{
		systemd:        &mockManager{},
		kubeadmCommand: "/nonexistent/kubeadm",
	}

	_, err := a.ApplyAction(context.Background(), buildRequest(t))
	if err == nil {
		t.Fatal("expected error when kubeadm binary doesn't exist")
	}
}

func TestNodeResetAction_KubeadmResetFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeKubeadm := newFakeKubeadm(t, dir, 1) // exits with code 1

	a := &nodeResetAction{
		systemd:        &mockManager{},
		kubeadmCommand: fakeKubeadm,
	}

	_, err := a.ApplyAction(context.Background(), buildRequest(t))
	if err == nil {
		t.Fatal("expected error when kubeadm reset exits non-zero")
	}
}

func TestNodeResetAction_MaskKubeletFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeKubeadm := newFakeKubeadm(t, dir, 0)

	a := &nodeResetAction{
		systemd: &mockManager{
			getUnitStatusFn: func(_ context.Context, _ string) (dbus.UnitStatus, error) {
				return dbus.UnitStatus{ActiveState: systemd.UnitActiveStateActive}, nil
			},
			stopUnitFn: func(_ context.Context, _ string) error {
				return nil
			},
			disableUnitFn: func(_ context.Context, _ string) error {
				return nil
			},
			maskUnitFn: func(_ context.Context, _ string) error {
				return os.ErrPermission
			},
		},
		kubeadmCommand: fakeKubeadm,
	}

	_, err := a.ApplyAction(context.Background(), buildRequest(t))
	if err == nil {
		t.Fatal("expected error when masking kubelet fails")
	}
}

func TestNodeResetAction_UnitNotFound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeKubeadm := newFakeKubeadm(t, dir, 0)

	// When the unit does not exist, EnsureUnitMasked is a no-op.
	a := &nodeResetAction{
		systemd: &mockManager{
			getUnitStatusFn: func(_ context.Context, _ string) (dbus.UnitStatus, error) {
				return dbus.UnitStatus{}, systemd.ErrUnitNotFound
			},
		},
		kubeadmCommand: fakeKubeadm,
	}

	resp, err := a.ApplyAction(context.Background(), buildRequest(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.GetItem() == nil {
		t.Fatal("expected non-nil response with item")
	}
}

func TestNodeResetAction_Idempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakeKubeadm := newFakeKubeadm(t, dir, 0)

	callCount := 0
	a := &nodeResetAction{
		systemd: &mockManager{
			getUnitStatusFn: func(_ context.Context, _ string) (dbus.UnitStatus, error) {
				return dbus.UnitStatus{}, systemd.ErrUnitNotFound
			},
		},
		kubeadmCommand: fakeKubeadm,
	}

	// Call twice — both should succeed.
	for i := 0; i < 2; i++ {
		callCount++
		resp, err := a.ApplyAction(context.Background(), buildRequest(t))
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", callCount, err)
		}
		if resp == nil {
			t.Fatalf("call %d: expected non-nil response", callCount)
		}
	}
}
