package provider

import (
	"context"
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/containerops"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// ActiveMachine pairs the selected nspawn slot with its applied state.
type ActiveMachine struct {
	Name  string
	State *State
}

// OperatorFactory binds a daemon operator to its runtime provider.
type OperatorFactory func(ctx context.Context, cfg *config.Config, store StateStore, worker NodeProvider) (Operator, error)

// HostServices supplies shared host-agent installation and filesystem operations.
type HostServices struct {
	CopyExecutable          func(source string, destination string) error
	FilesHaveEqualSHA256    func(first string, second string) (bool, error)
	RemoveHostHelpers       func(log *slog.Logger) phases.Task
	RemoveIfPresent         func(path string) error
	SaveState               func(store StateStore, state *State) phases.Task
	SyncDirs                func(dirs ...string) error
	RecoveryScriptPathUnder func(root string) string
}

// NSpawnServices supplies host integration for the nspawn runtime.
type NSpawnServices struct {
	HostServices
	NewOperator            OperatorFactory
	CheckMachinesTarget    func(log *slog.Logger, failForMissing bool) preflight.Checker
	ConvergeContainerAgent func(ctx context.Context, log *slog.Logger, state StateLoader) (func(ctx context.Context) error, error)
	EnsureLifecycleUnits   func(log *slog.Logger) phases.Task
	ConfigureDependencies  func(log *slog.Logger, rootFS *goalstates.RootFS, services []string) phases.Task
	InstallBinary          func(root isolation.NodeRoot) phases.Task
	CleanupLegacyBridgeCNI func(log *slog.Logger) phases.Task
	ActiveMachineFromStore func(ctx context.Context, store StateStore) (*ActiveMachine, error)
}

// ContainerServices supplies host integration for the OCI worker runtime.
type ContainerServices struct {
	HostServices
	NewOperator      OperatorFactory
	NewNode          func(params containerops.Params) *containerops.Node
	CheckBootstrap   func(ctx context.Context, log *slog.Logger) error
	SetupHost        func(cfg *config.Config, log *slog.Logger) phases.Task
	StartNode        func(params StartParams) phases.Task
	ResetNode        func(log *slog.Logger, teardown *containerops.Teardown) phases.Task
	RewriteNodeUnits func(ctx context.Context, log *slog.Logger, cfg *config.Config, state *State, startup ContainerStartupReader) error
}

// NoneServices supplies host-agent integration for the host-native runtime.
type NoneServices struct {
	HostServices
	NewOperator OperatorFactory
}
