// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package noneops composes Unbounded's public none backend and node phases.
// Adapted from Azure/unbounded cmd/agent/internal/noneops at 7e929e099f76,
// whose internal visibility prevents an embedding agent from importing it.
// Flex persists the applied config in its daemon state and owns the upgrade
// journal; the backend owns /opt/unbounded/node and its host systemd units.
package noneops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	fsutil "github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// Slot is the slot name a none node records its applied config under.
const Slot = goalstates.NSpawnMachineKube1

// Params bind a Node to one agent configuration.
type Params struct {
	Log *slog.Logger
	// Config is the agent configuration goal states are resolved from.
	Config *config.AgentConfig
	// Generation is the AKS settings version associated with Config: the
	// requested version for Upgrade, or the applied version for recovery.
	Generation string
	// ArtifactVersion identifies application-owned binaries installed by
	// Install. Empty retains the version identity of installations that did
	// not record application artifacts separately.
	ArtifactVersion string
	// Completion is the caller's frozen applied state, published with startup
	// intent before kubelet can expose this configuration to workloads.
	Completion         json.RawMessage
	PreviousCompletion json.RawMessage
	PreviousGeneration string
	// Downloads optionally overrides artifact download sources.
	Downloads *goalstates.DownloadOverrides
	// GoalState, when set, is used instead of resolving one. Bootstrap passes
	// the goal state it has already resolved so GPU and device discovery happens
	// once.
	GoalState *goalstates.MachineGoalState

	// Previous is the configuration the node runs with before an Upgrade: the
	// applied config. A rollback restores its units and host networking, so the
	// previous version starts on what it ran with. PreviousGoalState, when set,
	// is used instead of resolving Previous. With neither, a rollback uses this
	// configuration's.
	Previous          *config.AgentConfig
	PreviousGoalState *goalstates.MachineGoalState
	// PreviousArtifactVersion is the application identity that accompanied
	// Previous. Empty also preserves a legacy installation's directory name.
	PreviousArtifactVersion string

	// Stage prepares image archives after the source has stopped.
	Stage func(context.Context) error
	// Install adds application-owned artifacts to each prepared version.
	Install func(context.Context, *goalstates.MachineGoalState) error

	// Overrides and Hooks are left zero in production, which is the real host.
	// Tests set them to run a node against a temporary tree and a fake host.
	Overrides
	hostnode.Hooks
}

// Overrides are the none-specific seams of Params.
type Overrides struct {
	// Layout is the agent-owned directory. The zero value is the default.
	Layout none.Layout
	// SystemdDir is where units are written. Empty means /etc/systemd/system.
	SystemdDir string
	// Run executes host commands. Nil uses the real host.
	Run none.Runner
	// Proc is the procfs Destroy scans for container shims. Empty means /proc.
	Proc string
	// CgroupRoot is the cgroup v2 mount Destroy kills the pods' cgroups under.
	// Empty means /sys/fs/cgroup when Proc is also empty, and none otherwise.
	CgroupRoot string

	// Download replaces the artifact installers. Nil downloads for real.
	Download func(ctx context.Context) error
	// SetupGPU replaces the host GPU setup that runs before the stack starts.
	SetupGPU func(ctx context.Context) error
	// WaitKubelet overrides the kubelet readiness wait. Nil uses the phase the
	// nspawn mode uses.
	WaitKubelet func(ctx context.Context) error
	// ImageArchiveDir is the host directory staged container image archives
	// are imported from after containerd starts. Empty means
	// goalstates.ContainerImageArchiveHostDir.
	ImageArchiveDir string
	// ReapPods replaces scoped process/cgroup cleanup in filesystem tests.
	ReapPods func() error
}

// Node is a none node bound to one configuration.
type Node struct {
	p Params

	once    sync.Once
	gs      *goalstates.MachineGoalState
	err     error
	backend *none.Backend
	version string
}

// New returns a Node. Goal state resolution is deferred to first use so that
// operations that never need it (Stop, Status) work without a configuration.
func New(p Params) *Node {
	return &Node{p: p}
}

// Version is the version directory name of this configuration.
func (n *Node) Version() (string, error) {
	if err := n.resolve(); err != nil {
		return "", err
	}

	return n.version, nil
}

// GoalState is the resolved goal state with the host-native overrides applied.
func (n *Node) GoalState() (*goalstates.MachineGoalState, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.gs, nil
}

// Backend is the none backend wired to this node's phases.
func (n *Node) Backend() (*none.Backend, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend, nil
}

func (n *Node) resolve() error {
	n.once.Do(func() {
		n.gs, n.err = n.resolveGoalState()
		if n.err != nil {
			return
		}

		n.version = VersionFor(n.gs.RootFS, n.p.ArtifactVersion)

		n.backend, n.err = n.newBackend()
		if n.err != nil {
			return
		}

		n.applyOverrides()
	})

	return n.err
}

func (n *Node) resolveGoalState() (*goalstates.MachineGoalState, error) {
	return hostnode.GoalStateSource{
		Log: n.p.Log, Config: n.p.Config, Downloads: n.p.Downloads, GoalState: n.p.GoalState, Resolve: n.p.Resolve,
	}.For(Slot)
}

// applyOverrides points the goal state at the host: the root maps installer
// directories into the version, commands run on the host, and the binaries the
// generated configuration names are reached through the current link.
func (n *Node) applyOverrides() {
	root := n.root()

	n.gs.RootFS.RootOverride = root
	n.gs.NodeStart.RootOverride = root
	n.gs.NodeStart.ExecOverride = func(ctx context.Context, _ *slog.Logger, args ...string) (string, error) {
		res, err := n.backend.Exec(ctx, root, isolation.ExecSpec{Args: args})

		return res.Stdout, err
	}

	bin := n.p.Layout.BinDir()
	n.gs.NodeStart.Containerd.ContainerdBinPath = filepath.Join(bin, "containerd")
	n.gs.NodeStart.Containerd.RuncBinaryPath = filepath.Join(bin, "runc")
	n.gs.NodeStart.Kubelet.KubeletBinPath = filepath.Join(bin, "kubelet")
}

func (n *Node) root() none.Root {
	return none.Root{Layout: n.p.Layout, Version: n.version, HostRoot: n.p.HostRoot}
}

// VersionFor names the version directory for a rootfs goal state: the
// Kubernetes version plus a digest of everything else that decides which
// binaries are installed, so changing only the containerd or CNI version, the
// architecture, application artifact version or a download override yields a
// new directory while unchanged artifacts reuse the installed one.
func VersionFor(r *goalstates.RootFS, artifactVersion string) string {
	inputs := struct {
		Containerd, RunC, CNI, CoreDNS, Arch string
		CoreDNSEnabled                       bool
		Downloads                            *goalstates.DownloadOverrides
		Application                          string `json:",omitempty"`
	}{r.ContainerdVersion, r.RunCVersion, r.CNIPluginVersion, r.LocalDNS.CoreDNSVersion, r.HostArch, r.LocalDNS.Enabled, r.Downloads, artifactVersion}

	data, _ := json.Marshal(inputs) //nolint:errcheck // plain data always marshals
	sum := sha256.Sum256(data)

	return strings.TrimPrefix(r.KubernetesVersion, "v") + "-" + hex.EncodeToString(sum[:4])
}

// setupGPU prepares the host for GPU workloads. It does nothing on a node
// without NVIDIA GPUs.
func (n *Node) setupGPU(ctx context.Context) error {
	if n.p.SetupGPU != nil {
		return n.p.SetupGPU(ctx)
	}

	return nodestart.SetupNVIDIAOnHost(n.p.Log, n.gs.NodeStart).Do(ctx)
}

// gpuSetup is what containerd runs before each start on a GPU node, so a
// host reboot regenerates the CDI spec as a start by the agent does. It is
// empty on a node without NVIDIA GPUs.
func (n *Node) gpuSetup() [][]string {
	if !n.gs.NodeStart.Nvidia.Required {
		return nil
	}

	return nodestart.NVIDIAHostCDICommands(n.gs.NodeStart.NvidiaToolkit)
}

func (n *Node) waitLocalDNS(ctx context.Context) error {
	return n.p.AwaitLocalDNS(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) waitKubelet(ctx context.Context) error {
	if n.p.WaitKubelet != nil {
		return n.p.WaitKubelet(ctx)
	}

	return nodestart.WaitForKubeletOnHost(n.p.Log, n.gs.NodeStart, none.UnitKubelet).Do(ctx)
}

func (n *Node) newBackend() (*none.Backend, error) {
	proc, cgroups := n.p.Proc, n.p.CgroupRoot
	if proc == "" {
		proc = "/proc"

		if cgroups == "" {
			cgroups = "/sys/fs/cgroup"
		}
	}

	return &none.Backend{
		Log:        n.p.Log,
		Layout:     n.p.Layout,
		SystemdDir: n.p.SystemdDir,
		Run:        n.p.Run,
		Proc:       proc,
		CgroupRoot: cgroups,
		Units: none.UnitSpec{
			KubeletConfigPath: goalstates.KubeletConfigurationPath,
			LocalDNS:          n.gs.NodeStart.LocalDNS.Enabled,
			GPUSetup:          n.gpuSetup(),
		},
		Install: n.install,
		// The stack is the host: GPU devices and libraries are already visible,
		// so there is nothing to bind. What GPU workloads need is the toolkit's
		// CDI spec, rebuilt at every start from the devices present then.
		BeforeStart:  n.setupGPU,
		HostRoot:     n.p.HostRoot,
		GPUHostPaths: nodestart.NVIDIAToolkitPaths(n.gs.NodeStart.NvidiaToolkit),
		AfterStart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
			none.UnitContainerd: func(ctx context.Context) error {
				return nodestart.ImportContainerImagesFrom(n.p.Log, n.gs.NodeStart, n.p.ImageArchiveDir).Do(ctx)
			},
		},
		AfterRestart: map[string]func(context.Context) error{
			none.UnitLocalDNS: n.waitLocalDNS,
		},
	}, nil
}

// install places the version's artifacts: the same Kubernetes, CRI, CNI and
// CoreDNS installers the nspawn rootfs uses, pointed at the host root, plus the
// kernel modules the node needs and the LocalDNS supervisor script.
func (n *Node) install(ctx context.Context, _ isolation.NodeRoot) error {
	rf := n.gs.RootFS

	if n.p.Download != nil {
		if err := n.p.Download(ctx); err != nil {
			return err
		}

		return n.finishInstall(ctx)
	}

	tasks := []phases.Task{
		rootfs.DownloadKubeBinaries(n.p.Log, rf),
		rootfs.DownloadCRIBinaries(n.p.Log, rf),
		rootfs.DownloadCNIBinaries(n.p.Log, rf),
		rootfs.InstallCoreDNS(n.p.Log, rf),
		n.kernelModulesTask(),
	}

	if err := phases.Parallel(n.p.Log, tasks...).Do(ctx); err != nil {
		return err
	}

	return n.finishInstall(ctx)
}

func (n *Node) finishInstall(ctx context.Context) error {
	if n.p.Install != nil {
		if err := n.p.Install(ctx, n.gs); err != nil {
			return err
		}
	}
	if !n.gs.RootFS.LocalDNS.Enabled {
		return nil
	}

	return n.writeSupervisorWith(rootfs.LocalDNSSupervisorScript(filepath.Join(n.p.Layout.BinDir(), "coredns")))
}

// writeSupervisorWith replaces the supervisor script with a new file. The
// version being prepared can be the active one, whose supervisor a running
// LocalDNS unit is reading; truncating it in place would change what that
// shell executes, while a rename leaves the open file as it was.
func (n *Node) writeSupervisorWith(script []byte) error {
	path := n.gs.RootFS.Root().Path(goalstates.LocalDNSSupervisorPath)
	// LocalDNS's dynamic user executes the script, so libexec must be
	// traversable whatever the umask.
	if err := none.MkdirTraversable(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create LocalDNS supervisor directory: %w", err)
	}

	if err := fsutil.WriteFileDurable(path, script, 0o755); err != nil {
		return fmt.Errorf("write LocalDNS supervisor: %w", err)
	}

	return os.Chmod(path, 0o755) //nolint:gosec // executable by the service user
}

// Prepare installs the version and writes the units. It does not activate.
func (n *Node) Prepare(ctx context.Context) (isolation.NodeRoot, error) {
	if err := n.resolve(); err != nil {
		return nil, err
	}

	return n.backend.Prepare(ctx, n.nodeSpec())
}

// Provision installs the version, writes the units and points the current link
// at it. Bootstrap uses it for the first install, where there is no previous
// version to keep running.
func (n *Node) Provision(ctx context.Context) error {
	r, err := n.Prepare(ctx)
	if err != nil {
		return err
	}

	if _, err := n.backend.Activate(r); err != nil {
		return fmt.Errorf("activate %s: %w", r.Slot(), err)
	}

	return nil
}

// Configure writes the node configuration at its host paths and prepares the
// shared LocalDNS network. It reports whether containerd or kubelet
// configuration differed from what was on disk, so a reapply against a running
// node restarts only what changed.
func (n *Node) Configure(ctx context.Context) (containerdChanged, kubeletChanged bool, err error) {
	if err := n.resolve(); err != nil {
		return false, false, err
	}

	gs := n.gs.NodeStart
	containerd := nodestart.ConfigureContainerdOnHost(gs, none.ContainerdConfigPath)
	kubelet := nodestart.ConfigureKubeletOnHost(gs, none.UnitKubelet)

	if err := phases.Parallel(n.p.Log, containerd, kubelet, nodestart.ConfigureLocalDNS(gs)).Do(ctx); err != nil {
		return false, false, err
	}

	if err := n.setupNetwork(ctx); err != nil {
		return false, false, err
	}

	return containerd.Changed(), kubelet.Changed(), nil
}

// Running reports whether the whole stack is active.
func (n *Node) Running(ctx context.Context) (bool, error) {
	if err := n.resolve(); err != nil {
		return false, err
	}

	return hostnode.Running(ctx, n.backend, n.root())
}

// Start brings the stack up and waits for kubelet to bootstrap. When the stack
// was already running and its configuration changed, the changed service is
// restarted, containerd first.
func (n *Node) Start(ctx context.Context, wasRunning, containerdChanged, kubeletChanged bool) error {
	if err := n.resolve(); err != nil {
		return err
	}

	r := n.root()

	if err := n.backend.Start(ctx, r); err != nil {
		return err
	}

	err := hostnode.RestartChanged(ctx, n.p.Log, wasRunning,
		[]hostnode.ChangedUnit{{Unit: none.UnitContainerd, Changed: containerdChanged}, {Unit: none.UnitKubelet, Changed: kubeletChanged}},
		func(ctx context.Context, unit string) error {
			if _, err := n.backend.Exec(ctx, r, isolation.ExecSpec{Args: []string{"systemctl", "restart", unit}}); err != nil {
				return fmt.Errorf("systemctl restart %s: %w", unit, err)
			}

			return nil
		})
	if err != nil {
		return err
	}

	return n.waitBootstrap(ctx)
}

// waitBootstrap waits for kubelet's client credentials to appear. It runs
// through a serial group so a failure carries the task name the Machine
// condition reason is classified from.
func (n *Node) waitBootstrap(ctx context.Context) error {
	return phases.Serial(n.p.Log, nodestart.WaitForKubeletBootstrapOnHost(n.p.Log, n.gs.NodeStart)).Do(ctx)
}

// Restart restarts the node stack in place (soft reboot): LocalDNS, containerd,
// then kubelet, waiting for LocalDNS after it restarts and for kubelet to be
// active at the end. The host is not rebooted.
func (n *Node) Restart(ctx context.Context) error {
	if err := n.resolve(); err != nil {
		return err
	}

	if err := n.backend.Restart(ctx, n.root()); err != nil {
		return err
	}

	return n.waitKubelet(ctx)
}

// Upgrade prepares and switches a node after the RP's drain/delete signal.
// Failures before kubelet exposure may restore the source. A durable startup
// intent precedes either kubelet's start; all subsequent recovery is forward-only.
func (n *Node) Upgrade(ctx context.Context) error {
	return n.upgradePrepared(ctx)
}

// Commit drops rollback data after the caller persists applied state. Startup
// intent remains until the completed registration is observed on the recreated
// Node, so a newer goal cannot consume this operation's deletion signal.
func (n *Node) Commit() error {
	if err := confsnap.Discard(n.configSnapshotDir()); err != nil {
		return fmt.Errorf("remove the saved configuration: %w", err)
	}

	return n.removeJournal()
}

// RecoverUpgrade resumes an exposed startup without disturbing workloads.
// Without startup intent it may restore the source; callers must first observe
// Node absence so that destructive recovery respects the RP contract.
func (n *Node) RecoverUpgrade(ctx context.Context) error {
	startup, err := n.PendingStartup()
	if err != nil {
		return err
	}
	if startup != nil {
		return n.ResumeStartup(ctx)
	}
	j, err := n.readJournal()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := n.resolve(); err != nil {
		return err
	}
	current, err := n.p.Layout.Current()
	if err != nil {
		return err
	}
	if j.TargetGeneration != "" && j.TargetGeneration == n.p.Generation &&
		j.TargetConfig != "" && j.TargetConfig == configDigest(n.p.Config) && j.TargetArtifactVersion == n.p.ArtifactVersion && current == j.Target {
		n.version = current
		n.applyOverrides()
		if err := n.publishStartup(false); err != nil {
			return err
		}
		return n.ResumeStartup(ctx)
	}
	if j.Previous == "" {
		return fmt.Errorf("upgrade journal %s names no source version", n.journalPath())
	}
	source, err := n.previous()
	if err != nil {
		return err
	}
	return source.restoreSource(ctx, j.Previous)
}

// upgradeJournal is what Upgrade records before it changes anything.
type upgradeJournal struct {
	// Previous is the version the node ran before the upgrade.
	Previous string `json:"previous"`
	// Target is the version the upgrade installs.
	Target string `json:"target"`
	// TargetConfig is the configDigest of the configuration the upgrade
	// applies, which its caller persists as the applied config once the
	// upgrade is healthy. Empty when the node was built without one; such an
	// upgrade is never taken as finished.
	TargetConfig string `json:"targetConfig,omitempty"`
	// TargetGeneration identifies this repave even when its configuration and
	// binaries are unchanged. Without it recovery cannot prove completion.
	TargetGeneration string `json:"targetGeneration,omitempty"`
	// TargetArtifactVersion must also be persisted before recovery commits a
	// switch that changed only application binaries.
	TargetArtifactVersion string `json:"targetArtifactVersion,omitempty"`
}

// configDigest identifies an agent configuration by the SHA-256 of its JSON
// encoding, the encoding the applied config is persisted and read back in. It
// is empty for no configuration.
func configDigest(cfg *config.AgentConfig) string {
	if cfg == nil {
		return ""
	}

	data, _ := json.Marshal(cfg) //nolint:errcheck // plain data always marshals
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func (n *Node) journalPath() string {
	return filepath.Join(filepath.Dir(n.configSnapshotDir()), "none-upgrade-journal.json")
}

func (n *Node) readJournal() (upgradeJournal, error) {
	var j upgradeJournal

	data, err := os.ReadFile(n.journalPath())
	if err != nil {
		return j, err
	}

	if err := json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("decode upgrade journal %s: %w", n.journalPath(), err)
	}

	return j, nil
}

func (n *Node) writeJournal(j upgradeJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}

	if err := fsutil.WriteFileDurable(n.journalPath(), data, 0o600); err != nil {
		return fmt.Errorf("write upgrade journal: %w", err)
	}

	return nil
}

func (n *Node) removeJournal() error {
	if err := os.Remove(n.journalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove upgrade journal: %w", err)
	}

	return fsutil.SyncDir(filepath.Dir(n.journalPath()))
}

// previous is the node the configuration ran with before an upgrade.
func (n *Node) previous() (*Node, error) {
	if n.p.Previous == nil && n.p.PreviousGoalState == nil {
		return n, nil
	}

	p := n.p
	p.Config, p.GoalState, p.Previous, p.PreviousGoalState = n.p.Previous, n.p.PreviousGoalState, nil, nil
	p.ArtifactVersion, p.PreviousArtifactVersion = n.p.PreviousArtifactVersion, ""
	p.Generation, p.Completion = n.p.PreviousGeneration, n.p.PreviousCompletion
	p.PreviousGeneration, p.PreviousCompletion = "", nil

	prev := New(p)
	if err := prev.resolve(); err != nil {
		return nil, fmt.Errorf("resolve the configuration the node runs: %w", err)
	}

	return prev, nil
}

// unitSpec is the unit spec this configuration's units are written with.
func (n *Node) unitSpec() none.UnitSpec {
	return n.backend.Units
}

// resetHostNetwork cleans host networking a stopped stack left and sets it up
// for this configuration.
func (n *Node) resetHostNetwork(ctx context.Context) error {
	if err := n.cleanupNetwork(ctx); err != nil {
		return err
	}

	return n.setupNetwork(ctx)
}

func (n *Node) nodeSpec() isolation.NodeSpec {
	return isolation.NodeSpec{
		Slot:    n.version,
		Version: n.gs.RootFS.KubernetesVersion,
		Grants:  n.gs.RootFS.Grants(),
	}
}

// configSnapshotPaths includes application-owned dependencies and configuration
// alongside generated drop-ins. NPD binaries are versioned, but its config is shared.
func configSnapshotPaths() []string {
	return append(confsnap.SharedPaths(),
		none.DropInDir(goalstates.SystemdSystemDir, none.UnitContainerd),
		none.DropInDir(goalstates.SystemdSystemDir, none.UnitLocalDNS),
		"/etc/node-problem-detector/kernel-monitor.json",
		filepath.Join(goalstates.SystemdSystemDir, goalstates.LocalDNSSliceUnit),
	)
}

func (n *Node) configSnapshotDir() string {
	if n.p.ConfigSnapshotDir != "" {
		return n.p.ConfigSnapshotDir
	}
	return filepath.Join(n.p.HostRoot, "/etc/aks-flex-node/none-upgrade-config")
}

// PruneVersions removes every installed version except the active one. It runs
// after an upgrade succeeds, mirroring the removal of the old nspawn machine.
func (n *Node) PruneVersions() error {
	versions, err := n.p.Layout.Versions()
	if err != nil {
		return err
	}

	var errs []error

	for _, v := range versions {
		if err := n.p.Layout.Remove(v); err != nil && !errors.Is(err, none.ErrVersionActive) {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

func (n *Node) setupNetwork(ctx context.Context) error {
	return n.p.SetupLocalDNSNetwork(ctx, n.p.Log, n.gs.NodeStart)
}

func (n *Node) cleanupNetwork(ctx context.Context) error {
	return n.p.CleanupHostNetwork(ctx, n.p.Log)
}

func (n *Node) kernelModulesTask() phases.Task {
	return LoadKernelModules(n.p.Log, n.p.Run)
}

func execSpec(args ...string) isolation.ExecSpec { return isolation.ExecSpec{Args: args} }
