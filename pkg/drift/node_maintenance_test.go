package drift

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/sirupsen/logrus"
)

type fakeNodeMaintenance struct {
	cordoned bool

	cordonErr   error
	drainErr    error
	uncordonErr error
	getErr      error

	calls []string
}

func (f *fakeNodeMaintenance) IsCordoned(ctx context.Context, nodeName string) (bool, error) {
	_ = ctx
	_ = nodeName
	f.calls = append(f.calls, "IsCordoned")
	return f.cordoned, f.getErr
}

func (f *fakeNodeMaintenance) Cordon(ctx context.Context, nodeName string) error {
	_ = ctx
	_ = nodeName
	f.calls = append(f.calls, "Cordon")
	if f.cordonErr != nil {
		return f.cordonErr
	}
	f.cordoned = true
	return nil
}

func (f *fakeNodeMaintenance) Drain(ctx context.Context, nodeName string) error {
	_ = ctx
	_ = nodeName
	f.calls = append(f.calls, "Drain")
	return f.drainErr
}

func (f *fakeNodeMaintenance) Uncordon(ctx context.Context, nodeName string) error {
	_ = ctx
	_ = nodeName
	f.calls = append(f.calls, "Uncordon")
	if f.uncordonErr != nil {
		return f.uncordonErr
	}
	f.cordoned = false
	return nil
}

func TestCordonAndDrainExecutor_CordonsThenDrains_UncordonAfter(t *testing.T) {
	t.Parallel()

	ops := &fakeNodeMaintenance{cordoned: false}
	state := &cordonDrainState{}
	logger := logrus.New()

	cd := newCordonAndDrainExecutor("cordon-and-drain", logger, ops, state)
	if err := cd.Execute(context.Background()); err != nil {
		t.Fatalf("Execute err=%v, want nil", err)
	}

	wantCalls := []string{"IsCordoned", "Cordon", "Drain"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", ops.calls, wantCalls)
	}

	nodeName, _ := os.Hostname()
	if !state.shouldUncordon(nodeName) {
		t.Fatalf("shouldUncordon=false, want true")
	}

	u := newUncordonExecutor("uncordon", logger, ops, state)
	if err := u.Execute(context.Background()); err != nil {
		t.Fatalf("uncordon Execute err=%v, want nil", err)
	}

	if got := ops.calls[len(ops.calls)-1]; got != "Uncordon" {
		t.Fatalf("last call=%q, want Uncordon", got)
	}
}

func TestCordonAndDrainExecutor_AlreadyCordoned_DoesNotUncordon(t *testing.T) {
	t.Parallel()

	ops := &fakeNodeMaintenance{cordoned: true}
	state := &cordonDrainState{}
	logger := logrus.New()

	cd := newCordonAndDrainExecutor("cordon-and-drain", logger, ops, state)
	if err := cd.Execute(context.Background()); err != nil {
		t.Fatalf("Execute err=%v, want nil", err)
	}

	wantCalls := []string{"IsCordoned", "Drain"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", ops.calls, wantCalls)
	}

	u := newUncordonExecutor("uncordon", logger, ops, state)
	if err := u.Execute(context.Background()); err != nil {
		t.Fatalf("uncordon Execute err=%v, want nil", err)
	}

	for _, c := range ops.calls {
		if c == "Uncordon" {
			t.Fatalf("unexpected Uncordon call; calls=%v", ops.calls)
		}
	}
}

func TestCordonAndDrainExecutor_CordonFails_DoesNotDrain(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	ops := &fakeNodeMaintenance{cordoned: false, cordonErr: boom}
	state := &cordonDrainState{}
	logger := logrus.New()

	cd := newCordonAndDrainExecutor("cordon-and-drain", logger, ops, state)
	err := cd.Execute(context.Background())
	if err == nil {
		t.Fatalf("err=nil, want %v", boom)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want to contain %v", err, boom)
	}

	wantCalls := []string{"IsCordoned", "Cordon"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", ops.calls, wantCalls)
	}
}

func TestCordonAndDrainExecutor_DrainFails_UncordonsIfCordonedByUs(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	ops := &fakeNodeMaintenance{cordoned: false, drainErr: boom}
	state := &cordonDrainState{}
	logger := logrus.New()

	cd := newCordonAndDrainExecutor("cordon-and-drain", logger, ops, state)
	err := cd.Execute(context.Background())
	if err == nil {
		t.Fatalf("err=nil, want %v", boom)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want to contain %v", err, boom)
	}

	wantCalls := []string{"IsCordoned", "Cordon", "Drain", "Uncordon"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", ops.calls, wantCalls)
	}
	if ops.cordoned {
		t.Fatalf("cordoned=true, want false")
	}

	nodeName, _ := os.Hostname()
	if state.shouldUncordon(nodeName) {
		t.Fatalf("shouldUncordon=true, want false")
	}
}

func TestCordonAndDrainExecutor_DrainFails_DoesNotUncordonIfAlreadyCordoned(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	ops := &fakeNodeMaintenance{cordoned: true, drainErr: boom}
	state := &cordonDrainState{}
	logger := logrus.New()

	cd := newCordonAndDrainExecutor("cordon-and-drain", logger, ops, state)
	err := cd.Execute(context.Background())
	if err == nil {
		t.Fatalf("err=nil, want %v", boom)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err=%v, want to contain %v", err, boom)
	}

	wantCalls := []string{"IsCordoned", "Drain"}
	if !reflect.DeepEqual(ops.calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", ops.calls, wantCalls)
	}
	if !ops.cordoned {
		t.Fatalf("cordoned=false, want true")
	}
}

func TestShouldRetryWithAdmin(t *testing.T) {
	t.Parallel()

	tcs := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "forbidden wrapped as string",
			err:  errors.New("error when waiting for pod \"x\" to terminate: pods \"x\" is forbidden: User \"system:node:free-node\" cannot get resource \"pods\""),
			want: true,
		},
		{
			name: "unauthorized wrapped as string",
			err:  errors.New("Unauthorized"),
			want: true,
		},
		{
			name: "x509 cert expired",
			err:  errors.New("Get \"https://10.0.0.1:443\": x509: certificate has expired or is not yet valid"),
			want: true,
		},
		{
			name: "tls bad certificate",
			err:  errors.New("remote error: tls: bad certificate"),
			want: true,
		},
		{
			name: "other error",
			err:  errors.New("context deadline exceeded"),
			want: false,
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldRetryWithAdmin(tc.err); got != tc.want {
				t.Fatalf("shouldRetryWithAdmin(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
