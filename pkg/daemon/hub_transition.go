package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// errHubConverge marks a failure while updating an already-enabled Hub; kubelet
// stays routed through it.
var errHubConverge = errors.New("hub converge")

type hubPhase string

const (
	hubPrepare hubPhase = "prepare"
	hubRoute   hubPhase = "route"
	hubVerify  hubPhase = "verify"
	hubRestore hubPhase = "restore"
	hubCleanup hubPhase = "cleanup"
	hubStop    hubPhase = "stop"
)

// hubTransitionState is a local write-ahead journal. Applied is the last completed
// intent, not proof of the current routing while Phase is nonempty. Scope binds
// the journal to a cluster/node/worker generation; its encoding is adapter-owned.
// A replacement worker must not silently inherit another generation's progress.
type hubTransitionState struct {
	Version  int      `json:"version"`
	Scope    string   `json:"scope"`
	Accepted *bool    `json:"accepted"`
	Applied  *bool    `json:"applied,omitempty"`
	Phase    hubPhase `json:"phase,omitempty"`
}

type hubTransitionStore interface {
	Load(context.Context) (*hubTransitionState, error)
	Save(context.Context, *hubTransitionState) error
}

// hubTransitionOperations are the per-worker steps (pkg/yurthub.Operations in
// production). Each must be idempotent: a step can succeed before its checkpoint
// is saved, so it may run again after a crash.
type hubTransitionOperations interface {
	// Prepare installs and starts Hub without redirecting kubelet.
	Prepare(context.Context) error
	// Route switches kubelet to Hub.
	Route(context.Context) error
	// Verify confirms the live kubelet uses Hub and initializes Node metadata.
	Verify(context.Context) error
	// Restore returns kubelet to the direct API before any Hub teardown.
	Restore(context.Context) error
	// Cleanup undoes workload-facing changes; nothing today with filters off.
	Cleanup(context.Context) error
	// Stop stops Hub and deletes its cache.
	Stop(context.Context) error
}

// hubConverger is implemented by operations that can bring an already-enabled
// Hub up to date (binary, arguments, unit) after an agent upgrade.
type hubConverger interface {
	Converge(context.Context) error
}

type hubTransition struct {
	store hubTransitionStore
	ops   hubTransitionOperations
}

func newHubTransition(store hubTransitionStore, ops hubTransitionOperations) (*hubTransition, error) {
	if store == nil {
		return nil, fmt.Errorf("hub transition store is nil")
	}
	if ops == nil {
		return nil, fmt.Errorf("hub transition operations are nil")
	}
	return &hubTransition{store: store, ops: ops}, nil
}

// Reconcile runs inside the existing serialized daemon queue. A nil intent means
// no authoritative update: retain state without enabling, disabling or resuming
// an interrupted transition. After reconnect the adapter must fetch current intent
// before retrying. Local offline service recovery belongs to worker systemd.
func (r *hubTransition) Reconcile(ctx context.Context, scope string, intent *bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if intent == nil {
		return nil
	}
	if scope == "" {
		return fmt.Errorf("hub transition requires scope")
	}
	state, err := r.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load hub transition: %w", err)
	}
	if state == nil {
		if !*intent {
			// Acceptance is journaled before any operation runs, so a missing
			// journal means this worker never started an agent-owned transition.
			// Record disabled without teardown: with omitted=false, every existing
			// node reads false before RP enables DT, and that must be a no-op.
			disabled, applied := false, false
			if err := r.store.Save(ctx, &hubTransitionState{Version: 2, Scope: scope, Accepted: &disabled, Applied: &applied}); err != nil {
				return fmt.Errorf("record hub disabled: %w", err)
			}
			return nil
		}
		state = &hubTransitionState{Version: 2, Scope: scope}
	} else {
		if err := state.validate(); err != nil {
			return err
		}
		if state.Scope != scope {
			return fmt.Errorf("hub transition scope changed; explicit replacement handling required")
		}
		// Never mutate a store-owned snapshot before Save succeeds.
		copy := *state
		state = &copy
	}

	transitioned := false
	if state.Accepted == nil || *state.Accepted != *intent {
		transitioned = true
		accepted := *intent
		state.Accepted = &accepted
		if *intent {
			state.Phase = hubPrepare
		} else {
			// Even if the last completed state was disabled, an interrupted
			// enable may already have changed routing and needs full restoration.
			state.Phase = hubRestore
		}
		if err := r.store.Save(ctx, state); err != nil {
			return fmt.Errorf("accept hub intent: %w", err)
		}
	}

	for state.Phase != "" {
		transitioned = true
		if err := ctx.Err(); err != nil {
			return err
		}
		phase := state.Phase
		if err := r.run(ctx, phase); err != nil {
			return fmt.Errorf("hub transition %s: %w", phase, err)
		}
		next := *state
		next.Phase = nextHubPhase(phase)
		if next.Phase == "" {
			applied := *next.Accepted
			next.Applied = &applied
		}
		if err := r.store.Save(ctx, &next); err != nil {
			return fmt.Errorf("checkpoint hub transition %s: %w", phase, err)
		}
		state = &next
	}
	// A steady enabled Hub follows the current agent build. Skipped when this
	// read ran or resumed a transition, which already rendered everything.
	if !transitioned && state.Phase == "" && *state.Accepted {
		if converger, ok := r.ops.(hubConverger); ok {
			if err := converger.Converge(ctx); err != nil {
				return fmt.Errorf("%w: %w", errHubConverge, err)
			}
		}
	}
	return nil
}

func (r *hubTransition) run(ctx context.Context, phase hubPhase) error {
	switch phase {
	case hubPrepare:
		return r.ops.Prepare(ctx)
	case hubRoute:
		return r.ops.Route(ctx)
	case hubVerify:
		return r.ops.Verify(ctx)
	case hubRestore:
		return r.ops.Restore(ctx)
	case hubCleanup:
		return r.ops.Cleanup(ctx)
	case hubStop:
		return r.ops.Stop(ctx)
	default:
		return fmt.Errorf("unknown hub transition phase %q", phase)
	}
}

func nextHubPhase(phase hubPhase) hubPhase {
	switch phase {
	case hubPrepare:
		return hubRoute
	case hubRoute:
		return hubVerify
	case hubRestore:
		return hubCleanup
	case hubCleanup:
		return hubStop
	default:
		return ""
	}
}

func (s *hubTransitionState) validate() error {
	if s.Version != 2 || s.Scope == "" || s.Accepted == nil {
		return fmt.Errorf("invalid hub transition journal version, scope or intent")
	}
	switch s.Phase {
	case "":
		if s.Applied == nil || *s.Applied != *s.Accepted {
			return fmt.Errorf("idle hub journal does not match applied intent")
		}
	case hubPrepare, hubRoute, hubVerify:
		if !*s.Accepted {
			return fmt.Errorf("hub enable phase has disabled intent")
		}
	case hubRestore, hubCleanup, hubStop:
		if *s.Accepted {
			return fmt.Errorf("hub disable phase has enabled intent")
		}
	default:
		return fmt.Errorf("unknown hub transition phase %q", s.Phase)
	}
	return nil
}

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
