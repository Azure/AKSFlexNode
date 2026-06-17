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
	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/node-problem-detector.service
var serviceTemplate string

const (
	systemdUnitNPD = "node-problem-detector.service"
)

var tmpl = template.Must(template.New("npd-service").Parse(serviceTemplate))

type startTask struct {
	log            *slog.Logger
	apiServer      string
	kubeconfigPath string
	machineDir     string
	machineName    string
	nodeName       string
}

// Start returns a task that renders the NPD systemd unit file into the
// nspawn machine rootfs and ensures the service is running inside the
// container via systemd-run --machine.
func Start(cfg *config.Config, log *slog.Logger, machineDir, machineName string) phases.Task {
	return &startTask{
		log:            log,
		apiServer:      cfg.Node.Kubelet.ServerURL,
		kubeconfigPath: goalstates.KubeletKubeconfigPath,
		machineDir:     machineDir,
		machineName:    machineName,
		nodeName:       cfg.Agent.NodeName,
	}
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
		"NPDBinaryPath":  npdBinaryPath,
		"APIServerURL":   t.apiServer,
		"KubeconfigPath": t.kubeconfigPath,
		"NPDConfigPath":  npdConfigPath,
		"NodeName":       t.nodeName,
	}); err != nil {
		return false, fmt.Errorf("render npd service template: %w", err)
	}

	hostServicePath := filepath.Join(t.machineDir, "etc/systemd/system", systemdUnitNPD)

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
	// Check if the unit is already active inside the container.
	_, err := utilexec.MachineRun(ctx, t.log, t.machineName,
		"systemctl", "is-active", systemdUnitNPD)

	switch {
	case err != nil:
		// Unit not active (or not loaded). Reload and start.
		if _, reloadErr := utilexec.MachineRun(ctx, t.log, t.machineName,
			"systemctl", "daemon-reload"); reloadErr != nil {
			return fmt.Errorf("daemon-reload in machine %s: %w", t.machineName, reloadErr)
		}
		if _, startErr := utilexec.MachineRun(ctx, t.log, t.machineName,
			"systemctl", "enable", "--now", systemdUnitNPD); startErr != nil {
			return fmt.Errorf("start npd in machine %s: %w", t.machineName, startErr)
		}
		return nil
	default:
		if restart {
			if _, restartErr := utilexec.MachineRun(ctx, t.log, t.machineName,
				"systemctl", "restart", systemdUnitNPD); restartErr != nil {
				return fmt.Errorf("restart npd in machine %s: %w", t.machineName, restartErr)
			}
		}
		return nil
	}
}
