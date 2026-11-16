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
	"strconv"
	"strings"
	"text/template"
	"time"

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

var (
	tmpl             = template.Must(template.New("npd-service").Parse(serviceTemplate))
	errNPDNotRunning = errors.New("NPD unit is not running")
)

type startTask struct {
	log            *slog.Logger
	apiServer      string
	kubeconfigPath string
	root           isolation.NodeRoot
	nodeName       string
	unit           string
	forceRestart   bool
	pollInterval   time.Duration
	stableFor      time.Duration
	binaryPath     string
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
		unit:           systemdUnitNPD,
		binaryPath:     npdBinaryPath,
		log:            log,
		apiServer:      nodeStart.Kubelet.APIServer,
		kubeconfigPath: goalstates.KubeletKubeconfigPath,
		root:           nodeStart.Root(),
		nodeName:       nodeStart.NodeName,
		run:            nodeStart.Exec,
		pollInterval:   time.Second,
		stableFor:      2 * time.Second,
	}
	if nodeStart.HostNative() {
		task.unit = HostUnitNPD
		task.binaryPath = filepath.Join(none.Layout{}.BinDir(), "node-problem-detector")
	} else if nodeStart.InContainer {
		task.binaryPath = "/usr/local/bin/node-problem-detector"
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("start NPD unit %s: %w", t.unit, err)
	}
	serviceUpdated, err := t.ensureServiceFile()
	if err != nil {
		return fmt.Errorf("ensure npd service file: %w", err)
	}

	if err := t.ensureSystemdUnit(ctx, serviceUpdated || t.forceRestart); err != nil {
		return err
	}
	return t.waitForRunning(ctx)
}

func (t *startTask) waitForRunning(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(t.pollInterval)
	defer ticker.Stop()

	var lastPID uint64
	var stableSince time.Time
	var lastStatus error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("NPD unit %s did not stay running (last MainPID=%d): %w", t.unit, lastPID, errors.Join(err, lastStatus))
		}
		pid, err := t.runningPID(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("wait for NPD unit %s: %w", t.unit, ctxErr)
		}
		if err != nil {
			if !errors.Is(err, errNPDNotRunning) {
				return err
			}
			lastPID, stableSince, lastStatus = 0, time.Time{}, err
		} else {
			lastStatus = nil
			if pid != lastPID {
				lastPID, stableSince = pid, time.Now()
			}
			// Type=simple may look running briefly before NPD exits. Let
			// systemd retry failures and require a stable process, not a start job.
			if time.Since(stableSince) >= t.stableFor {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("NPD unit %s did not stay running (last MainPID=%d): %w", t.unit, lastPID, errors.Join(ctx.Err(), lastStatus))
		case <-ticker.C:
		}
	}
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

func (t *startTask) runningPID(ctx context.Context) (uint64, error) {
	out, err := t.run(ctx, t.log, "systemctl", "show", "--property=ActiveState", "--property=SubState", "--property=MainPID", t.unit)
	if err != nil {
		return 0, fmt.Errorf("read NPD unit %s status: %w", t.unit, err)
	}
	properties := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[key] = value
		}
	}
	pid, err := strconv.ParseUint(properties["MainPID"], 10, 32)
	if err != nil || pid == 0 || properties["ActiveState"] != "active" || properties["SubState"] != "running" {
		return 0, fmt.Errorf("%w: %s (ActiveState=%q SubState=%q MainPID=%q)",
			errNPDNotRunning, t.unit, properties["ActiveState"], properties["SubState"], properties["MainPID"])
	}
	return pid, nil
}

func (t *startTask) ensureSystemdUnit(ctx context.Context, updated bool) error {
	_, activeErr := t.run(ctx, t.log, "systemctl", "is-active", t.unit)
	if updated || activeErr != nil {
		if _, err := t.run(ctx, t.log, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reload NPD unit %s: %w", t.unit, err)
		}
	}
	if activeErr != nil {
		if _, err := t.run(ctx, t.log, "systemctl", "enable", "--now", t.unit); err != nil {
			return fmt.Errorf("start NPD unit %s: %w", t.unit, err)
		}
	} else if updated {
		if _, err := t.run(ctx, t.log, "systemctl", "restart", t.unit); err != nil {
			return fmt.Errorf("restart NPD unit %s: %w", t.unit, err)
		}
	}
	return nil
}
