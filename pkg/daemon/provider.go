package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	agentconfig "github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// NodeProvider owns node-runtime actions. Select once at an entry boundary and
// retain it for the flow; applied metadata is validation, not a dispatch source.
type NodeProvider interface {
	BootstrapActions
	agentActions
	hostActions
	Mode() isolation.Mode
	NewOperator(context.Context, *config.Config, stateStore) (nodeOperator, error)
}

// BootstrapActions leaves AKS registration and state sequencing with the caller.
type BootstrapActions interface {
	ValidateEnvironment(containerized bool) error
	BootstrapPresent(*slog.Logger) bool
	CheckBootstrap(context.Context, *slog.Logger, *State) error
	ResolveGoalState(context.Context, *slog.Logger, *config.Config, string) (*agentconfig.AgentConfig, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, error)
	SetupHost(*config.Config, *slog.Logger, bool) phases.Task
	StartNode(*config.Config, *slog.Logger, string, *goalstates.MachineGoalState, *goalstates.ContainerImageArchiveStaging, stateStore, *State) phases.Task
	RuntimeChecks(*config.Config, *slog.Logger, bool) []preflight.Checker
}

type agentActions interface {
	ValidateAgentActivation(context.Context) error
	ValidateActiveSlot(string) error
	CheckAgentCandidate(context.Context, string) error
	SynchronizeAgent(source, slot string) error
	ValidateAgent(source, slot string) error
	RollbackAgent(source, slot string) error
}

type hostActions interface {
	WantedTargets() []string
	SupportsDisconnectTolerance() bool
	ResetNode(*slog.Logger) phases.Task
	ValidateResetState(*State) error
	RewriteNodeUnits(context.Context, *slog.Logger, *config.Config, stateStore) error
	// NSpawnRuntime is absent when the runtime has no nspawn lifecycle hooks.
	NSpawnRuntime() nspawnRuntime
}

type nspawnRuntime interface {
	ConvergeContainerAgent(context.Context, *slog.Logger, agentUpgradeStateLoader) (func(context.Context) error, error)
}

var nodeProviders = map[isolation.Mode]func() NodeProvider{
	isolation.ModeNSpawn: func() NodeProvider { return nspawnProvider{binaryPath: activeNspawnAgentBinaryPath} },
	isolation.ModeNone:   func() NodeProvider { return noneProvider{} },
}

// NewNodeProvider is the sole runtime registry. Empty mode retains the nspawn default.
func NewNodeProvider(mode isolation.Mode) (NodeProvider, error) {
	if mode == "" {
		mode = isolation.ModeNSpawn
	}
	create, ok := nodeProviders[mode]
	if !ok {
		return nil, fmt.Errorf("unsupported node isolation %q", mode)
	}
	return create(), nil
}

func validateAppliedProvider(provider NodeProvider, applied *State) error {
	if applied != nil && applied.isolationMode() != provider.Mode() {
		return fmt.Errorf("configured isolation %s differs from applied isolation %s; reset the node before changing modes", provider.Mode(), applied.isolationMode())
	}
	return nil
}

// CheckBootstrapIsolation runs before registration or host mutation, even when
// the optional preflight command was not run.
func CheckBootstrapIsolation(ctx context.Context, log *slog.Logger, provider NodeProvider) error {
	if err := provider.ValidateEnvironment(Containerized()); err != nil {
		return err
	}
	store, err := NewFileStateStore()
	if err != nil {
		return err
	}
	applied, err := store.Load(ctx)
	if err != nil {
		return err
	}
	return checkBootstrapProvider(ctx, log, provider, applied, registeredNodeProviders())
}

func checkBootstrapProvider(ctx context.Context, log *slog.Logger, provider NodeProvider, applied *State, candidates []NodeProvider) error {
	if err := validateAppliedProvider(provider, applied); err != nil {
		return err
	}
	// Registry discovery also catches partial installs without applied Flex state.
	for _, candidate := range candidates {
		if candidate.Mode() != provider.Mode() && candidate.BootstrapPresent(log) {
			return fmt.Errorf("host contains an installed or partially reset %s node; reset it before bootstrapping", candidate.Mode())
		}
	}
	return provider.CheckBootstrap(ctx, log, applied)
}

// discoverResetProvider loads Flex state first: SDK Destroy can remove all SDK
// footprints before later cleanup fails. A corrupt journal must never route to
// the legacy runtime's destructive cleanup.
func discoverResetProvider(ctx context.Context, load func(context.Context) (*State, error), log *slog.Logger, candidates []NodeProvider) (NodeProvider, error) {
	applied, err := load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load applied state for reset: %w", err)
	}
	provider, err := discoverNodeProvider(applied, log, candidates)
	if err != nil {
		return nil, err
	}
	if applied != nil {
		if err := provider.ValidateResetState(applied); err != nil {
			return nil, err
		}
	}
	return provider, nil
}

// discoverNodeProvider preserves applied ownership, including after SDK cleanup.
// Without applied state, runtime footprints can identify a partial bootstrap.
func discoverNodeProvider(applied *State, log *slog.Logger, candidates []NodeProvider) (NodeProvider, error) {
	if applied != nil {
		return NewNodeProvider(applied.isolationMode())
	}
	var found NodeProvider
	for _, provider := range candidates {
		if !provider.BootstrapPresent(log) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous runtime ownership: both %s and %s are present", found.Mode(), provider.Mode())
		}
		found = provider
	}
	if found != nil {
		return found, nil
	}
	return NewNodeProvider("")
}

func installedNodeProvider(ctx context.Context, log *slog.Logger) (NodeProvider, stateStore, error) {
	store, err := NewFileStateStore()
	if err != nil {
		return nil, nil, err
	}
	applied, err := store.Load(ctx)
	if err != nil {
		return nil, nil, err
	}
	provider, err := discoverNodeProvider(applied, log, registeredNodeProviders())
	if err != nil {
		return nil, nil, err
	}
	return provider, store, nil
}

func registeredNodeProviders() []NodeProvider {
	providers := make([]NodeProvider, 0, len(nodeProviders))
	for _, create := range nodeProviders {
		providers = append(providers, create())
	}
	return providers
}
