package yurthub

import (
	"context"
	"errors"
	"fmt"
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
// intent, not proof of the current routing while Phase is nonempty. Scope is the
// caller's worker identifier; the journal lives inside that worker's rootfs.
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

// hubTransitionOperations are the per-worker steps. Each must be idempotent:
// a step can succeed before its checkpoint is saved and run again after a crash.
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
// an interrupted transition. After reconnect the caller must fetch current intent
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
			// Every node reads false until RP enables DT; write nothing.
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
