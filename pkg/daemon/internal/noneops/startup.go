package noneops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/daemon/internal/hostnode"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/confsnap"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

// Startup is a forward-only intent. Once published, kubelet may have admitted
// workloads; recovery must not stop services, rewrite configuration or reap pods.
type Startup struct {
	RecordVersion   int                 `json:"recordVersion"`
	Version         string              `json:"version"`
	Config          *config.AgentConfig `json:"config"`
	Generation      string              `json:"generation"`
	ArtifactVersion string              `json:"artifactVersion,omitempty"`
	Completion      json.RawMessage     `json:"completion,omitempty"`
	Restored        bool                `json:"restored,omitempty"`
	Finished        bool                `json:"finished,omitempty"`
}

func (n *Node) startupPath() string {
	return filepath.Join(filepath.Dir(n.configSnapshotDir()), "none-startup.json")
}

func (n *Node) PendingStartup() (*Startup, error) {
	data, err := os.ReadFile(n.startupPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read none startup: %w", err)
	}
	var s Startup
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("decode none startup: %w", err)
	}
	if s.RecordVersion != 1 || s.Version == "" || s.Config == nil {
		return nil, fmt.Errorf("invalid none startup record in %s", n.startupPath())
	}
	return &s, nil
}

func (n *Node) PendingUpgrade() (bool, error) {
	_, err := n.readJournal()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (n *Node) writeStartup(s *Startup) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode none startup: %w", err)
	}
	return utilio.WriteFileDurable(n.startupPath(), data, 0o600)
}

func (n *Node) publishStartup(restored bool) error {
	if err := n.syncPrepared(n.version); err != nil {
		return fmt.Errorf("persist prepared node: %w", err)
	}
	return n.writeStartup(&Startup{
		RecordVersion: 1, Version: n.version, Config: n.p.Config,
		Generation: n.p.Generation, ArtifactVersion: n.p.ArtifactVersion,
		Completion: n.p.Completion, Restored: restored,
	})
}

func (n *Node) FinishStartup() error {
	s, err := n.PendingStartup()
	if err != nil {
		return err
	}
	if s == nil {
		return errors.New("no none startup to finish")
	}
	// kubelet enablement happens after the exposure marker. Persist its boot
	// symlinks before the caller can clear the last recovery record.
	if err := n.syncPrepared(s.Version); err != nil {
		return err
	}
	s.Finished = true
	return n.writeStartup(s)
}

// ClearStartup is called only after persistence and observation of the recreated
// Node. Until then its absence must not authorize repaving a newer RP goal.
func (n *Node) ClearStartup() error {
	if err := os.Remove(n.startupPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return utilio.SyncDir(filepath.Dir(n.startupPath()))
}

func (n *Node) ResumeStartup(ctx context.Context) error {
	s, err := n.PendingStartup()
	if err != nil || s == nil {
		return err
	}
	current, err := n.p.Layout.Current()
	if err != nil {
		return err
	}
	if current != s.Version {
		return fmt.Errorf("none startup expects active version %s, found %s; refusing to switch live binaries", s.Version, current)
	}
	p := n.p
	p.Config, p.GoalState = s.Config, nil
	p.Generation, p.ArtifactVersion, p.Completion = s.Generation, s.ArtifactVersion, s.Completion
	running := New(p)
	if err := running.resolve(); err != nil {
		return err
	}
	// Persisted version names are authoritative even if library defaults or
	// offline download resolution changed since preparation.
	running.version = s.Version
	running.applyOverrides()
	return running.startPrepared(ctx, nil)
}

func (n *Node) command(ctx context.Context, args ...string) error {
	_, err := n.backend.Exec(ctx, n.root(), execSpec(args...))
	return err
}

// startPrepared never restarts an active service and never rewrites node files.
// beforeKubelet publishes the durable exposure boundary on first startup.
func (n *Node) startPrepared(ctx context.Context, beforeKubelet func() error) error {
	units := []string{none.UnitContainerd, none.UnitKubelet}
	if _, err := os.Lstat(filepath.Join(unitDir(n.p.SystemdDir), none.UnitLocalDNS)); err == nil {
		units = append([]string{none.UnitLocalDNS}, units...)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, unit := range units {
		if unit == none.UnitKubelet && beforeKubelet != nil {
			if err := beforeKubelet(); err != nil {
				return fmt.Errorf("record kubelet startup: %w", err)
			}
		}
		active := n.command(ctx, "systemctl", "is-active", "--quiet", unit) == nil
		if !active {
			if unit == none.UnitContainerd {
				if err := n.setupGPU(ctx); err != nil {
					return err
				}
			}
			if err := n.command(ctx, "systemctl", "enable", "--now", unit); err != nil {
				return fmt.Errorf("start %s: %w", unit, err)
			}
		}
		if gate := n.backend.AfterStart[unit]; gate != nil && (!active || unit == none.UnitLocalDNS) {
			if err := gate(ctx); err != nil {
				return err
			}
		}
	}
	if err := n.waitKubelet(ctx); err != nil {
		return err
	}
	return n.waitBootstrap(ctx)
}

func (n *Node) writePreparedUnits(ctx context.Context) error {
	spec := n.unitSpec()
	spec.BinDir = n.p.Layout.BinDir()
	if !spec.LocalDNS {
		path := filepath.Join(unitDir(n.p.SystemdDir), none.UnitLocalDNS)
		if _, err := os.Lstat(path); err == nil {
			if err := n.command(ctx, "systemctl", "disable", none.UnitLocalDNS); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if _, err := none.WriteUnits(unitDir(n.p.SystemdDir), spec); err != nil {
		return err
	}
	if _, err := none.WriteDropIns(unitDir(n.p.SystemdDir), spec); err != nil {
		return err
	}
	return n.command(ctx, "systemctl", "daemon-reload")
}

func (n *Node) upgradePrepared(ctx context.Context) (err error) {
	if err := n.resolve(); err != nil {
		return err
	}
	if s, err := n.PendingStartup(); err != nil || s != nil {
		return errors.Join(err, errors.New("none startup is pending completion"))
	}
	if pending, err := n.PendingUpgrade(); err != nil || pending {
		return errors.Join(err, errors.New("an earlier none upgrade needs recovery"))
	}
	prev, err := n.previous()
	if err != nil {
		return err
	}
	current, err := n.p.Layout.Current()
	if err != nil || current == "" {
		return errors.Join(err, errors.New("none upgrade has no active source version"))
	}
	if err := n.writeJournal(upgradeJournal{
		Previous: current, Target: n.version, TargetConfig: configDigest(n.p.Config),
		TargetGeneration: n.p.Generation, TargetArtifactVersion: n.p.ArtifactVersion,
	}); err != nil {
		return err
	}
	if _, err := hostnode.TakeSnapshotOf(n.p.Log, n.configSnapshotDir(), n.p.HostRoot, configSnapshotPaths()); err != nil {
		return errors.Join(err, n.removeJournal())
	}
	stopped := false
	defer func() {
		if err == nil {
			return
		}
		// Publication can succeed before fsync reports an error. Only proven
		// absence permits destructive rollback.
		if s, readErr := n.PendingStartup(); readErr != nil || s != nil {
			err = errors.Join(err, readErr)
			return
		}
		if !stopped {
			// No configuration was changed before Stop. An incomplete stop
			// must not invent a fresh registration for a still-active source.
			return
		}
		rollbackCtx := context.WithoutCancel(ctx)
		err = errors.Join(err, prev.restoreSource(rollbackCtx, current))
	}()
	if err := n.backend.Stop(ctx, n.root()); err != nil {
		return err
	}
	stopped = true
	if err := n.reapPods(); err != nil {
		return err
	}
	if n.p.Stage != nil {
		if err := n.p.Stage(ctx); err != nil {
			return err
		}
	}
	if _, err := n.backend.PrepareVersion(ctx, n.nodeSpec()); err != nil {
		return err
	}
	if _, err := n.backend.Activate(n.root()); err != nil {
		return err
	}
	if err := n.writePreparedUnits(ctx); err != nil {
		return err
	}
	if err := n.cleanupNetwork(ctx); err != nil {
		return err
	}
	if err := hostnode.ResetCNIState(n.p.HostRoot); err != nil {
		return err
	}
	if _, _, err := n.Configure(ctx); err != nil {
		return err
	}
	if err := hostnode.ResetKubeletIdentity(n.gs.NodeStart); err != nil {
		return err
	}
	return n.startPrepared(ctx, func() error { return n.publishStartup(false) })
}

func (n *Node) restoreSource(ctx context.Context, version string) error {
	if err := n.resolve(); err != nil {
		return err
	}
	if err := n.backend.Stop(ctx, n.root()); err != nil {
		return err
	}
	if err := n.reapPods(); err != nil {
		return err
	}
	if _, err := n.p.Layout.Flip(version); err != nil {
		return err
	}
	n.version = version
	n.applyOverrides()
	if err := n.writePreparedUnits(ctx); err != nil {
		return err
	}
	if confsnap.Exists(n.configSnapshotDir()) {
		if err := hostnode.RestoreSnapshot(n.p.Log, n.configSnapshotDir(), n.p.HostRoot); err != nil {
			return err
		}
	}
	// Restoring the source also recreates its Node. Its new pod CIDR may
	// differ, and every old pod was reaped; neither CNI config nor IPAM
	// reservations can be reused. Rotated kubelet credentials stay restored.
	if err := hostnode.ResetCNIState(n.p.HostRoot); err != nil {
		return err
	}
	if err := n.resetHostNetwork(ctx); err != nil {
		return err
	}
	// Restore the source registration fence before exposing its kubelet, even
	// when its older snapshot predates generation labels.
	if err := nodestart.ConfigureKubeletOnHost(n.gs.NodeStart, none.UnitKubelet).Do(ctx); err != nil {
		return err
	}
	if err := n.command(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// A rollback source can also admit new workloads. Its next recovery must
	// resume startup rather than execute this destructive restoration again.
	return n.startPrepared(ctx, func() error { return n.publishStartup(true) })
}
