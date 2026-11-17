package runtimeprovider

import (
	"log/slog"

	"github.com/Azure/unbounded/pkg/agent/phases"
)

// activeMachine pairs the selected nspawn slot with its applied state.
type activeMachine struct {
	Name  string
	State *State
}

// HostServices supplies the host-agent facilities shared with worker runtimes.
type HostServices struct {
	PlannedHostRoot            func() string
	ActivationLockPath         string
	CopyExecutable             func(source string, destination string) error
	FilesHaveEqualSHA256       func(first string, second string) (bool, error)
	RemoveHostHelpers          func(log *slog.Logger) phases.Task
	RemoveIfPresent            func(path string) error
	SaveState                  func(store StateStore, state *State) phases.Task
	SyncDirs                   func(dirs ...string) error
	RecoveryScriptPathUnder    func(root string) string
	UninstallService           func(log *slog.Logger) phases.Task
	EnsureNSpawnLifecycleUnits func(log *slog.Logger) phases.Task
	CleanupLegacyBridgeCNI     func(log *slog.Logger) phases.Task
}
