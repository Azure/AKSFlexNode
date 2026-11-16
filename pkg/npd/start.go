package npd

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/template"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/node-problem-detector.service
var serviceTemplate string

const (
	systemdUnitNPD = "node-problem-detector.service"
	HostUnitNPD    = "aks-flex-node-npd.service"
)

var tmpl = template.Must(template.New("npd-service").Parse(serviceTemplate))

type startTask struct {
	log            *slog.Logger
	apiServer      string
	kubeconfigPath string
	root           isolation.NodeRoot
	nodeName       string
	unit           string
	binaryPath     string
	forceRestart   bool
	run            func(context.Context, *slog.Logger, ...string) (string, error)
}

// Start renders and starts NPD through the node's filesystem and executor.
func Start(log *slog.Logger, cfg *config.Config, nodeStart *goalstates.NodeStart) phases.Task {
	if disabledForOfflineArtifacts(cfg) {
		if nodeStart.HostNative() {
			return &disableHostTask{log: log, root: nodeStart.Root(), run: nodeStart.Exec}
		}
		return disabledTask{name: "start-npd", log: log}
	}

	task := &startTask{
		log:            log,
		apiServer:      nodeStart.Kubelet.APIServer,
		kubeconfigPath: goalstates.KubeletKubeconfigPath,
		root:           nodeStart.Root(),
		nodeName:       nodeStart.NodeName,
		unit:           systemdUnitNPD,
		binaryPath:     npdBinaryPath,
		run:            nodeStart.Exec,
	}
	if nodeStart.HostNative() {
		task.unit = HostUnitNPD
		task.binaryPath = filepath.Join(none.Layout{}.BinDir(), "node-problem-detector")
	}
	return task
}

// Restart makes the service pick up the active version's binary even when its
// unit text is unchanged after a host-native version-directory switch.
func Restart(log *slog.Logger, cfg *config.Config, nodeStart *goalstates.NodeStart) phases.Task {
	task := Start(log, cfg, nodeStart)
	if start, ok := task.(*startTask); ok {
		start.forceRestart = true
	}
	return task
}

type disableHostTask struct {
	log  *slog.Logger
	root isolation.NodeRoot
	run  func(context.Context, *slog.Logger, ...string) (string, error)
}

func (t *disableHostTask) Name() string { return "start-npd" }

func (t *disableHostTask) Do(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disable NPD unit %s: %w", HostUnitNPD, err)
	}
	unitPath := t.root.Path(filepath.Join("/etc/systemd/system", HostUnitNPD))
	if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat NPD unit %s: %w", HostUnitNPD, err)
	}

	// Keep the unit file as the ownership record; stop and disable can be
	// retried even after the active version no longer contains NPD.
	for _, action := range []string{"stop", "disable"} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("disable NPD unit %s: %w", HostUnitNPD, err)
		}
		if _, err := t.run(ctx, t.log, "systemctl", action, HostUnitNPD); err != nil {
			return fmt.Errorf("%s NPD unit %s: %w", action, HostUnitNPD, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disable NPD unit %s: %w", HostUnitNPD, err)
	}
	t.log.Info("Host NPD unit stopped and disabled for offline artifacts", "unit", HostUnitNPD)
	return nil
}

func (t *startTask) Name() string { return "start-npd" }

func (t *startTask) Do(ctx context.Context) error {
	serviceUpdated, err := t.ensureServiceFile()
	if err != nil {
		return fmt.Errorf("ensure npd service file: %w", err)
	}

	return t.ensureSystemdUnit(ctx, serviceUpdated || t.forceRestart)
}

func (t *startTask) ensureServiceFile() (updated bool, err error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]any{
		"NPDBinaryPath":  t.binaryPath,
		"APIServerURL":   t.apiServer,
		"KubeconfigPath": t.kubeconfigPath,
		"NPDConfigPath":  npdConfigPath,
		"NodeName":       t.nodeName,
	}); err != nil {
		return false, fmt.Errorf("render npd service template: %w", err)
	}

	hostServicePath := t.root.Path(filepath.Join("/etc/systemd/system", t.unit))

	current, err := os.ReadFile(hostServicePath) //nolint:gosec // path is constructed, not user input
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fall through to create
	case err != nil:
		return false, err
	default:
		if bytes.Equal(bytes.TrimSpace(current), bytes.TrimSpace(buf.Bytes())) {
			return false, nil
		}
	}

	if err := utilio.InstallFile(hostServicePath, &buf, 0o644); err != nil { //nolint:gosec // service files must be world-readable
		return false, err
	}
	return true, nil
}

func (t *startTask) ensureSystemdUnit(ctx context.Context, restart bool) error {
	_, err := t.run(ctx, t.log, "systemctl", "is-active", t.unit)

	switch {
	case err != nil:
		// Unit not active (or not loaded). Reload and start.
		if _, reloadErr := t.run(ctx, t.log, "systemctl", "daemon-reload"); reloadErr != nil {
			return fmt.Errorf("reload NPD unit %s: %w", t.unit, reloadErr)
		}
		if _, startErr := t.run(ctx, t.log, "systemctl", "enable", "--now", t.unit); startErr != nil {
			return fmt.Errorf("start NPD unit %s: %w", t.unit, startErr)
		}
		return nil
	default:
		if restart {
			if _, restartErr := t.run(ctx, t.log, "systemctl", "restart", t.unit); restartErr != nil {
				return fmt.Errorf("restart NPD unit %s: %w", t.unit, restartErr)
			}
		}
		return nil
	}
}
