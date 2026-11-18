package yurthub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// fileHubTransitionStore keeps the journal at path inside the worker rootfs root, so
// repave and reset delete it with the Hub state it describes. Access stays beneath
// root even through symlinks. It is separate from daemon-state.json so DT progress
// never touches the applied Machine settings version.
type fileHubTransitionStore struct {
	root string
	path string
}

func (s fileHubTransitionStore) Load(ctx context.Context) (*hubTransitionState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := s.read()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read hub transition journal: %w", err)
	}
	var state hubTransitionState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("decode hub transition journal: %w", err)
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s fileHubTransitionStore) Save(ctx context.Context, state *hubTransitionState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("hub transition state is nil")
	}
	if err := state.validate(); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode hub transition journal: %w", err)
	}
	if err := s.write(append(data, '\n')); err != nil {
		return fmt.Errorf("write hub transition journal: %w", err)
	}
	return nil
}

func (s fileHubTransitionStore) read() ([]byte, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck // directory handle
	return root.ReadFile(s.path)
}

func (s fileHubTransitionStore) write(data []byte) error {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // directory handle
	return utilio.WriteFileInRoot(root, s.path, data, 0o600)
}
