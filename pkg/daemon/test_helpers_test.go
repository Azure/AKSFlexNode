package daemon

import (
	"context"
	"io"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type testStateStore struct {
	state *runtimeprovider.State
	err   error
}

func (s *testStateStore) Load(ctx context.Context) (*runtimeprovider.State, error) {
	if s.state == nil || s.err != nil {
		return nil, s.err
	}
	loaded := *s.state
	if loaded.Isolation == "" {
		loaded.Isolation = isolation.ModeNSpawn
	}
	return &loaded, nil
}
func (s *testStateStore) Save(ctx context.Context, state *runtimeprovider.State) error { return nil }
func (s *testStateStore) Delete(ctx context.Context) error                             { return nil }
