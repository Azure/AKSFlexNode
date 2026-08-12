package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestStartupGate(t *testing.T) {
	t.Parallel()

	t.Run("blocks until opened", func(t *testing.T) {
		t.Parallel()
		gate := newStartupGate()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- gate.wait(ctx) }()

		select {
		case err := <-done:
			t.Fatalf("wait returned before open: %v", err)
		default:
		}
		gate.open()
		if err := <-done; err != nil {
			t.Fatalf("wait after open: %v", err)
		}
		gate.open() // idempotent
	})

	t.Run("honors cancellation", func(t *testing.T) {
		t.Parallel()
		gate := newStartupGate()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := gate.wait(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	})
}
