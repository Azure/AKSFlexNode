// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostnode holds what the composition roots of the isolation modes that
// run the node stack outside a systemd-nspawn machine (none today) share: resolving a goal state, the LocalDNS network steps, the restart of
// services whose configuration changed, the saved copy of the shared
// configuration, the reset of the kubelet identity and the unmounting of pod
// volumes. The modes differ in where commands run and in how the node is built
// and started; they do not differ in these.
package hostnode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	executil "github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// KubeletClientPKIDir holds kubelet's rotated client certificates. It is not
// goalstates.KubeletPKIDir, which is the cluster CA directory.
const KubeletClientPKIDir = "/var/lib/kubelet/pki"

// KubeletDir is the kubelet state directory. Pod volume mounts propagate to it.
const KubeletDir = "/var/lib/kubelet"

// GoalStateSource resolves the goal state a node is built from.
type GoalStateSource struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides
	// GoalState, when set, is used instead of resolving one. Bootstrap passes the
	// goal state it has already resolved so GPU and device discovery happens once.
	GoalState *goalstates.MachineGoalState
	// Resolve overrides goal state resolution. Tests use it; the default is
	// goalstates.ResolveMachine.
	Resolve func(slot string) (*goalstates.MachineGoalState, error)
}

// For returns the goal state for slot.
func (s GoalStateSource) For(slot string) (*goalstates.MachineGoalState, error) {
	if s.GoalState != nil {
		return s.GoalState, nil
	}

	resolve := s.Resolve
	if resolve == nil {
		if s.Config == nil {
			return nil, errors.New("no agent configuration to resolve the node")
		}

		resolve = func(slot string) (*goalstates.MachineGoalState, error) {
			return goalstates.ResolveMachine(s.Log, s.Config, slot, s.Downloads)
		}
	}

	gs, err := resolve(slot)
	if err != nil {
		return nil, fmt.Errorf("resolve machine goal state: %w", err)
	}

	return gs, nil
}

// Hooks are the seams a node exposes so tests can run it against a temporary
// tree and a fake host. Production leaves the zero value, which is the real
// host and the real phases.
type Hooks struct {
	// Resolve overrides goal state resolution. The default is
	// goalstates.ResolveMachine.
	Resolve func(slot string) (*goalstates.MachineGoalState, error)
	// HostRoot prefixes the same-path host locations (/etc, /var/lib, /opt/cni)
	// the node writes. Empty is the real host.
	HostRoot string
	// ConfigSnapshotDir is where Upgrade saves the shared configuration so a
	// rollback can restore it. Empty means a directory in the agent configuration
	// directory below HostRoot.
	ConfigSnapshotDir string
	// SetupNetwork and CleanupNetwork replace the shared LocalDNS network
	// phases, and WaitLocalDNS the LocalDNS readiness wait. WaitLocalDNS is
	// given the node start goal state whose listeners are waited on.
	SetupNetwork   func(ctx context.Context) error
	CleanupNetwork func(ctx context.Context) error
	WaitLocalDNS   func(ctx context.Context, ns *goalstates.NodeStart) error
}

// SetupLocalDNSNetwork prepares the shared LocalDNS network.
func (h Hooks) SetupLocalDNSNetwork(ctx context.Context, log *slog.Logger, ns *goalstates.NodeStart) error {
	if h.SetupNetwork != nil {
		return h.SetupNetwork(ctx)
	}

	return nodestart.SetupLocalDNSNetwork(log, ns).Do(ctx)
}

// CleanupHostNetwork removes host networking the stopped node left behind, as a
// repave does.
func (h Hooks) CleanupHostNetwork(ctx context.Context, log *slog.Logger) error {
	if h.CleanupNetwork != nil {
		return h.CleanupNetwork(ctx)
	}

	return reset.CleanupNetwork(log).Do(ctx)
}

// AwaitLocalDNS waits for LocalDNS to answer.
func (h Hooks) AwaitLocalDNS(ctx context.Context, log *slog.Logger, ns *goalstates.NodeStart) error {
	if h.WaitLocalDNS != nil {
		return h.WaitLocalDNS(ctx, ns)
	}

	return nodestart.WaitForLocalDNS(log, ns).Do(ctx)
}

// Running reports whether the node root's stack is up.
func Running(ctx context.Context, b isolation.Backend, root isolation.SlotRef) (bool, error) {
	st, err := b.Status(ctx, root)
	if err != nil {
		return false, err
	}

	return st.Running, nil
}

// ChangedUnit is a service and whether its configuration changed.
type ChangedUnit struct {
	Unit    string
	Changed bool
}

// RestartChanged restarts, in order, the units whose configuration changed. It
// does nothing when the node was not already running, since a start reads the
// configuration written before it. restart runs one unit's restart and owns the
// wording of its error; logAttrs are added to the log line for each restart.
func RestartChanged(ctx context.Context, log *slog.Logger, wasRunning bool, units []ChangedUnit, restart func(ctx context.Context, unit string) error, logAttrs ...any) error {
	if !wasRunning {
		return nil
	}

	for _, u := range units {
		if !u.Changed {
			continue
		}

		log.Info("configuration changed on a running node, restarting service", append([]any{"unit", u.Unit}, logAttrs...)...)

		if err := restart(ctx, u.Unit); err != nil {
			return err
		}
	}

	return nil
}

// Snapshot is the saved copy of the configuration files both stacks of a node
// share, taken before an upgrade rewrites them so a rollback can put them back
// (see confsnap).
type Snapshot struct {
	dir      string
	hostRoot string
	restored bool
	// reload makes systemd read the unit files and drop-ins again after they
	// were restored. Nil when the snapshot is not of the real root.
	reload func() error
}

// TakeSnapshot saves the shared configuration files below hostRoot into dir.
func TakeSnapshot(log *slog.Logger, dir, hostRoot string) (*Snapshot, error) {
	return TakeSnapshotOf(log, dir, hostRoot, confsnap.SharedPaths())
}

// TakeSnapshotOf is TakeSnapshot of the given paths, for a mode whose node
// stack writes more of the host in place (see confsnap.PathsFor).
func TakeSnapshotOf(log *slog.Logger, dir, hostRoot string, paths []string) (*Snapshot, error) {
	if err := confsnap.Take(dir, hostRoot, paths); err != nil {
		return nil, fmt.Errorf("save shared configuration: %w", err)
	}

	s := &Snapshot{dir: dir, hostRoot: hostRoot}
	if hostRoot == "" {
		s.reload = func() error { return reloadSystemd(log) }
	}

	return s, nil
}

// reloadSystemd asks the real systemd to read unit files and drop-ins again.
func reloadSystemd(log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := executil.OutputCmd(ctx, log, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	return nil
}

// Restore puts the saved files back and, on the real root, has systemd read
// the restored unit definitions. A failure keeps the snapshot so it can be tried
// again.
func (s *Snapshot) Restore() error {
	if err := confsnap.Restore(s.dir, s.hostRoot); err != nil {
		return fmt.Errorf("restore shared configuration: %w", err)
	}

	if s.reload != nil {
		if err := s.reload(); err != nil {
			return fmt.Errorf("reload systemd after restoring the shared configuration: %w", err)
		}
	}

	s.restored = true

	return nil
}

// RestoreSnapshot restores a snapshot saved in dir by an earlier process, such
// as one a crash interrupted, and reloads systemd for the real root.
func RestoreSnapshot(log *slog.Logger, dir, hostRoot string) error {
	s := &Snapshot{dir: dir, hostRoot: hostRoot}
	if hostRoot == "" {
		s.reload = func() error { return reloadSystemd(log) }
	}

	return s.Restore()
}

// Restored reports whether Restore has succeeded.
func (s *Snapshot) Restored() bool { return s.restored }

// ResetKubeletIdentity removes the previous kubelet client credentials when the
// node authenticates with a bootstrap token, so the next start requests new
// ones. Direct kubeconfig and exec credential modes are rewritten by Configure.
func ResetKubeletIdentity(ns *goalstates.NodeStart) error {
	k := ns.Kubelet
	if k.BootstrapToken == "" || len(k.KubeconfigData) > 0 || k.ExecCredential != nil {
		return nil
	}

	for _, p := range []string{goalstates.KubeletKubeconfigPath, KubeletClientPKIDir} {
		if err := os.RemoveAll(ns.Root().Path(p)); err != nil {
			return fmt.Errorf("remove previous kubelet credentials %s: %w", p, err)
		}
	}

	return nil
}

// CNIStateDir is where the CNI plugins keep per-node state, host-local IPAM's
// address reservations among it.
const CNIStateDir = "/var/lib/cni"

// ResetCNIState removes the CNI network configuration and the CNI state a
// stopped stack left below hostRoot, as a repave to a new nspawn machine starts
// without them. The configuration directory itself stays, empty.
//
// A repave deletes the Node, and the new one can be given a different pod CIDR.
// Left in place, the old configuration makes containerd report the network
// ready at once and starts pods with addresses from the old range until the
// CNI's own pod rewrites it, and host-local IPAM keeps reservations for pods
// that were ended. Every file in the directory goes, those of add-ons
// included: they are written by DaemonSet pods that write them again when they
// start, and a repave to a new nspawn machine loses them all the same.
func ResetCNIState(hostRoot string) error {
	confDir := filepath.Join(hostRoot, goalstates.CNIConfigDir)

	entries, err := os.ReadDir(confDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("list CNI configuration: %w", err)
	}

	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(confDir, e.Name())); err != nil {
			return fmt.Errorf("remove CNI configuration: %w", err)
		}
	}

	if err := os.RemoveAll(filepath.Join(hostRoot, CNIStateDir)); err != nil {
		return fmt.Errorf("remove CNI state: %w", err)
	}

	return nil
}
