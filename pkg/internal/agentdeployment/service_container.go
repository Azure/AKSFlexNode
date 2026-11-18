package agentdeployment

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

// containerServiceUnitContent is the agent's unit in its container: it runs the daemon in a
// container of the agent's image, which the host's provisioning names, or an AgentUpgrade
// selected.
//
//go:embed assets/aks-flex-node-agent-container.service
var containerServiceUnitContent []byte

// containerRecoveryUnitContent is the agent's recovery unit in its container, which restores the
// last-good image when an upgraded agent's unit keeps failing.
//
//go:embed assets/aks-flex-node-agent-recovery-container.service
var containerRecoveryUnitContent []byte

// containerNSpawnPreStartUnitContent and containerNSpawnPostStartUnitContent are the nspawn
// lifecycle units in the agent's container, which the machines' hooks start: they run the agent's
// nspawn-lifecycle phases in containers of its image, so no binary of the agent is on the host.
//
//go:embed assets/aks-flex-node-nspawn-pre-start-container@.service
var containerNSpawnPreStartUnitContent []byte

//go:embed assets/aks-flex-node-nspawn-post-start-container@.service
var containerNSpawnPostStartUnitContent []byte

// containerNSpawnLifecycleUnitAssets returns the container's nspawn lifecycle units, under the
// names the machines' hooks start; see config.NSpawnPreStartUnit.
func containerNSpawnLifecycleUnitAssets(systemdDir string) []agentServiceAsset {
	return []agentServiceAsset{
		{
			path:    filepath.Join(systemdDir, config.NSpawnPreStartUnit),
			content: containerNSpawnPreStartUnitContent,
			mode:    0o644,
		},
		{
			path:    filepath.Join(systemdDir, config.NSpawnPostStartUnit),
			content: containerNSpawnPostStartUnitContent,
			mode:    0o644,
		},
	}
}

// installContainerServiceTask installs, enables, and starts the agent's unit in its container,
// with its recovery unit, as installServiceTask does on a host. They have the same names, so
// UninstallService removes them in either case.
type installContainerServiceTask struct {
	log       *slog.Logger
	unitDir   string
	systemctl func(ctx context.Context, args ...string) error
}

func newInstallContainerServiceTask(log *slog.Logger) *installContainerServiceTask {
	return &installContainerServiceTask{
		log:     log,
		unitDir: SystemdSystemDir,
		systemctl: func(ctx context.Context, args ...string) error {
			return utilexec.RunCmd(ctx, log, utilexec.Systemctl(), args...)
		},
	}
}

func (t *installContainerServiceTask) Name() string { return "install-service" }

func (t *installContainerServiceTask) Do(ctx context.Context) error {
	if _, err := writeContainerUnits(t.unitDir, false); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", ServiceUnitName},
		{"start", ServiceUnitName},
	} {
		if err := t.systemctl(ctx, args...); err != nil {
			return fmt.Errorf("systemctl %s: %w", args[0], err)
		}
	}
	t.log.Info("systemd service installed and started", "unit", ServiceUnitName)
	return nil
}

// agentScopeAnnotations has runc set up a container's scope as the agent's units set up theirs,
// for unit, a unit name or %n in a unit: bound to the unit, so stopping it stops the container;
// killed 30 s into a stop, within the units' stop timeouts, so their stops end only once their
// containers have; and unloaded even when it fails, as a failed scope keeps the next container
// from its name, and on ACL nothing may reset it.
func agentScopeAnnotations(unit string) []string {
	return []string{
		"--annotation", fmt.Sprintf("org.systemd.property.BindsTo=['%s']", unit),
		"--annotation", "org.systemd.property.TimeoutStopUSec=uint64 30000000",
		"--annotation", "org.systemd.property.CollectMode='inactive-or-failed'",
	}
}

// writeContainerUnits writes the agent's units from its image, but with keepExisting, only those
// that are missing, and reports whether it changed any.
func writeContainerUnits(unitDir string, keepExisting bool) (bool, error) {
	changed := false
	// The agent's unit last, as on a host, so an interrupted write never leaves it pointing at a
	// recovery unit that isn't there.
	for _, unit := range []struct {
		name    string
		content []byte
	}{
		{
			name:    config.NSpawnPreStartUnit,
			content: containerNSpawnPreStartUnitContent,
		},
		{
			name:    config.NSpawnPostStartUnit,
			content: containerNSpawnPostStartUnitContent,
		},
		{
			name:    recoveryServiceUnitName,
			content: containerRecoveryUnitContent,
		},
		{
			name:    ServiceUnitName,
			content: containerServiceUnitContent,
		},
	} {
		name, content := unit.name, unit.content
		path := filepath.Join(unitDir, name)
		current, err := os.ReadFile(filepath.Clean(path)) // #nosec G304 -- path is built from the agent-owned systemd directory and fixed unit name
		if err == nil && (keepExisting || bytes.Equal(current, content)) {
			continue
		}
		if err := utilio.WriteFile(path, content, 0o644); err != nil {
			return changed, fmt.Errorf("write %s: %w", path, err)
		}
		changed = true
	}
	return changed, nil
}
