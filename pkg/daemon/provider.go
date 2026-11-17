package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/internal/runtimeprovider"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

var nodeProviders = map[isolation.Mode]func() runtimeprovider.NodeProvider{
	isolation.ModeNSpawn:    newNSpawnProvider,
	isolation.ModeNone:      newNoneProvider,
	isolation.ModeContainer: newContainerProvider,
}

func hostProviderServices() runtimeprovider.HostServices {
	return runtimeprovider.HostServices{
		CopyExecutable: copyExecutable, FilesHaveEqualSHA256: filesHaveEqualSHA256,
		RemoveHostHelpers: removeHostHelpers, RemoveIfPresent: removeIfPresent,
		SaveState: saveState, SyncDirs: syncDirs, RecoveryScriptPathUnder: recoveryScriptPathUnder,
	}
}

func newNSpawnProvider() runtimeprovider.NodeProvider {
	return runtimeprovider.NSpawn{BinaryPath: runtimeprovider.NSpawnAgentBinaryPath, Services: runtimeprovider.NSpawnServices{
		HostServices: hostProviderServices(), NewOperator: newNSpawnOperator,
		CheckMachinesTarget: CheckMachinesTarget, ConvergeContainerAgent: convergeNSpawnContainerAgent,
		EnsureLifecycleUnits: EnsureNSpawnLifecycleUnits, ConfigureDependencies: ConfigureNSpawnDependencies,
		CleanupLegacyBridgeCNI: cleanupLegacyBridgeCNI,
		ActiveMachineFromStore: activeMachineFromStore,
	}}
}

func newNoneProvider() runtimeprovider.NodeProvider {
	return runtimeprovider.None{Host: hostProviderServices(), OperatorFactory: newNoneOperator}
}

func convergeNSpawnContainerAgent(ctx context.Context, log *slog.Logger, state agentUpgradeStateLoader) (func(context.Context) error, error) {
	convergence := newContainerAgentConvergence(log, state)
	held, err := convergence.converge(ctx)
	if err != nil {
		return nil, err
	}
	if held {
		return convergence.writeUnits, nil
	}
	return noHeldAssets, nil
}

// NewNodeProvider constructs the runtime for an explicitly selected mode.
func NewNodeProvider(mode isolation.Mode) (runtimeprovider.NodeProvider, error) {
	create, ok := nodeProviders[mode]
	if !ok {
		return nil, fmt.Errorf("unsupported node isolation %q", mode)
	}
	return create(), nil
}

func validateAppliedProvider(provider runtimeprovider.NodeProvider, applied *runtimeprovider.State) error {
	if applied != nil && applied.Isolation != provider.Mode() {
		return fmt.Errorf("configured isolation %s differs from applied isolation %s; reset the node before changing modes", provider.Mode(), applied.Isolation)
	}
	return nil
}

// CheckBootstrapIsolation runs before registration or host mutation, even when
// the optional preflight command was not run.
func CheckBootstrapIsolation(ctx context.Context, log *slog.Logger, provider runtimeprovider.NodeProvider) error {
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

func checkBootstrapProvider(ctx context.Context, log *slog.Logger, provider runtimeprovider.NodeProvider, applied *runtimeprovider.State, candidates []runtimeprovider.NodeProvider) error {
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
func discoverResetProvider(ctx context.Context, store stateStore, log *slog.Logger, candidates []runtimeprovider.NodeProvider) (runtimeprovider.NodeProvider, error) {
	applied, err := store.Load(ctx)
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
func discoverNodeProvider(applied *runtimeprovider.State, log *slog.Logger, candidates []runtimeprovider.NodeProvider) (runtimeprovider.NodeProvider, error) {
	if applied != nil {
		return NewNodeProvider(applied.Isolation)
	}
	var found runtimeprovider.NodeProvider
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
	return NewNodeProvider(isolation.ModeNSpawn)
}

func installedNodeProvider(ctx context.Context, log *slog.Logger) (runtimeprovider.NodeProvider, stateStore, error) {
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

func registeredNodeProviders() []runtimeprovider.NodeProvider {
	providers := make([]runtimeprovider.NodeProvider, 0, len(nodeProviders))
	for _, create := range nodeProviders {
		providers = append(providers, create())
	}
	return providers
}
