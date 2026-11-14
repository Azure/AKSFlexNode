package nodelifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/containerops"
)

type fakeLifecycle struct {
	phase, slot string
	fail        bool
}

func (f *fakeLifecycle) run(phase, slot string) error {
	f.phase, f.slot = phase, slot
	if f.fail {
		return errors.New("hook failed")
	}
	return nil
}
func (f *fakeLifecycle) PreStart(_ context.Context, slot string) error {
	return f.run("pre-start", slot)
}
func (f *fakeLifecycle) PostStart(_ context.Context, slot string) error {
	return f.run("post-start", slot)
}
func (f *fakeLifecycle) Reconcile(_ context.Context, slot string) error {
	return f.run("reconcile", slot)
}

func TestNodeLifecycleDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		args          []string
		fail, wantErr bool
	}{
		{name: "pre start", args: []string{"pre-start", "kube1"}},
		{name: "post start", args: []string{"post-start", "kube2"}},
		{name: "reconcile", args: []string{"reconcile", "kube1"}},
		{name: "failure propagated", args: []string{"pre-start", "kube1"}, fail: true, wantErr: true},
		{name: "invalid slot", args: []string{"pre-start", "../kube1"}, wantErr: true},
		{name: "missing slot", args: []string{"post-start"}, wantErr: true},
		{name: "extra argument", args: []string{"post-start", "kube1", "kube2"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeLifecycle{fail: tc.fail}
			cmd := newCommand(func() lifecycle { return fake })
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if !cmd.Hidden {
				t.Fatal("internal lifecycle must be hidden")
			}
			if err := cmd.Execute(); (err != nil) != tc.wantErr {
				t.Fatalf("Execute = %v", err)
			}
			if !tc.wantErr && (fake.phase != tc.args[0] || fake.slot != tc.args[1]) {
				t.Fatalf("wrong hook: %+v", fake)
			}
		})
	}
}

func TestNodeLifecycleUsesSharedContainerGPUHooks(t *testing.T) {
	t.Parallel()
	lifecycle := newLifecycle(slog.New(slog.DiscardHandler), containerops.NVIDIAHooks{})
	controller, ok := lifecycle.Controller.(controller)
	if !ok || controller.backend.RefreshGrants == nil || lifecycle.AfterStart == nil || lifecycle.ReloadSystemd == nil {
		t.Fatal("container lifecycle is missing shared backend or GPU operations")
	}
	if lifecycle.Peer("kube1") != "kube2" || lifecycle.Peer("kube2") != "kube1" {
		t.Fatal("peer exclusion is not wired")
	}
}
