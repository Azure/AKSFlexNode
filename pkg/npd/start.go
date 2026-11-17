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
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/node-problem-detector.service
var serviceTemplate string

// UnitName is the NPD service unit in every node root.
const UnitName = "node-problem-detector.service"

var tmpl = template.Must(template.New("npd-service").Parse(serviceTemplate))

type startTask struct {
	log            *slog.Logger
	apiServer      string
	kubeconfigPath string
	root           isolation.NodeRoot
	nodeName       string
	binaryPath     string
	run            func(context.Context, *slog.Logger, ...string) (string, error)
}

// Start renders NPD through root and manages it through the node's executor.
// root maps the agent's filesystem. Only native execution uses its mapped
// binary path; guest executors use the canonical in-node path.
func Start(log *slog.Logger, cfg *config.Config, root isolation.NodeRoot, nodeStart *goalstates.NodeStart, mode isolation.Mode) phases.Task {
	if disabledForOfflineArtifacts(cfg) {
		return &disableTask{log: log, root: root, run: nodeStart.Exec}
	}

	binaryPath := BinaryPath
	if mode == isolation.ModeNone {
		binaryPath = root.Path(BinaryPath)
	}

	return &startTask{
		log:            log,
		apiServer:      nodeStart.Kubelet.APIServer,
		kubeconfigPath: goalstates.KubeletKubeconfigPath,
		root:           root,
		nodeName:       nodeStart.NodeName,
		binaryPath:     binaryPath,
		run:            nodeStart.Exec,
	}
}

type disableTask struct {
	log  *slog.Logger
	root isolation.NodeRoot
	run  func(context.Context, *slog.Logger, ...string) (string, error)
}

func (t *disableTask) Name() string { return "start-npd" }

func (t *disableTask) Do(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disable NPD unit %s: %w", UnitName, err)
	}
	unitPath := t.root.Path(filepath.Join("/etc/systemd/system", UnitName))
	if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("stat NPD unit %s: %w", UnitName, err)
	}

	// Keep the unit file as the ownership record; stop and disable can be
	// retried even after the active version no longer contains NPD.
	for _, action := range []string{"stop", "disable"} {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("disable NPD unit %s: %w", UnitName, err)
		}
		if _, err := t.run(ctx, t.log, "systemctl", action, UnitName); err != nil {
			return fmt.Errorf("%s NPD unit %s: %w", action, UnitName, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("disable NPD unit %s: %w", UnitName, err)
	}
	t.log.Info("NPD unit stopped and disabled for offline artifacts", "unit", UnitName)
	return nil
}

func (t *startTask) Name() string { return "start-npd" }

func (t *startTask) Do(ctx context.Context) error {
	serviceUpdated, err := t.ensureServiceFile()
	if err != nil {
		return fmt.Errorf("ensure npd service file: %w", err)
	}

	return t.ensureSystemdUnit(ctx, serviceUpdated)
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

	hostServicePath := t.root.Path(filepath.Join("/etc/systemd/system", UnitName))

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
	_, err := t.run(ctx, t.log, "systemctl", "is-active", UnitName)

	switch {
	case err != nil:
		// Unit not active (or not loaded). Reload and start.
		if _, reloadErr := t.run(ctx, t.log, "systemctl", "daemon-reload"); reloadErr != nil {
			return fmt.Errorf("reload NPD unit %s: %w", UnitName, reloadErr)
		}
		if _, startErr := t.run(ctx, t.log, "systemctl", "enable", "--now", UnitName); startErr != nil {
			return fmt.Errorf("start NPD unit %s: %w", UnitName, startErr)
		}
		return nil
	default:
		if restart {
			if _, restartErr := t.run(ctx, t.log, "systemctl", "restart", UnitName); restartErr != nil {
				return fmt.Errorf("restart NPD unit %s: %w", UnitName, restartErr)
			}
		}
		return nil
	}
}
