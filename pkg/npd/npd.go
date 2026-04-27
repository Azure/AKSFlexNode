package npd

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/template"

	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilhost"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---------------------------------------------------------------------------
// Download
// ---------------------------------------------------------------------------

const (
	defaultNPDURLTemplate = "https://github.com/kubernetes/node-problem-detector/releases/download/%s/node-problem-detector-%s-linux_%s.tar.gz"

	npdBinaryPath = "/usr/bin/node-problem-detector"
	npdConfigPath = "/etc/node-problem-detector/kernel-monitor.json"
)

type downloadTask struct {
	version string
}

// Download returns a task that downloads the node-problem-detector binary
// and config from the upstream GitHub release tarball.
func Download(cfg *config.Config) phases.Task {
	version := cfg.Npd.Version
	if version == "" {
		version = config.DefaultNPDVersion
	}
	return &downloadTask{version: version}
}

func (t *downloadTask) Name() string { return "download-npd" }

func (t *downloadTask) Do(ctx context.Context) error {
	if versionMatch(t.version) {
		return nil // already installed at correct version
	}

	downloadURL := constructDownloadURL(t.version)
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress npd tar: %w", err)
		}

		switch tarFile.Name {
		case "bin/node-problem-detector":
			if err := utilio.InstallFile(npdBinaryPath, tarFile.Body, 0o755); err != nil { //nolint:gosec // binary must be executable
				return fmt.Errorf("install npd binary: %w", err)
			}
		case "config/kernel-monitor.json":
			if err := utilio.InstallFile(npdConfigPath, tarFile.Body, 0o644); err != nil { //nolint:gosec // config must be readable
				return fmt.Errorf("install npd config: %w", err)
			}
		default:
			continue
		}
	}

	return nil
}

func constructDownloadURL(version string) string {
	arch := utilhost.GetArch()
	return fmt.Sprintf(defaultNPDURLTemplate, version, version, arch)
}

func versionMatch(expectedVersion string) bool {
	if !utilio.IsExecutable(npdBinaryPath) {
		return false
	}
	output, err := utilexec.New().Command(npdBinaryPath, "--version").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), expectedVersion)
}

// ---------------------------------------------------------------------------
// Start (systemd service)
// ---------------------------------------------------------------------------

//go:embed node-problem-detector.service
var serviceTemplate string

const (
	systemdUnitNPD = "node-problem-detector.service"
	servicePath    = "/etc/systemd/system/node-problem-detector.service"
)

var tmpl = template.Must(template.New("npd-service").Parse(serviceTemplate))

type startTask struct {
	apiServer      string
	kubeconfigPath string
	systemd        systemd.Manager
}

// Start returns a task that renders the NPD systemd unit file and ensures
// the service is running.
func Start(cfg *config.Config) phases.Task {
	return &startTask{
		apiServer:      cfg.Node.Kubelet.ServerURL,
		kubeconfigPath: config.KubeletKubeconfigPath,
		systemd:        systemd.New(),
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
	}); err != nil {
		return false, fmt.Errorf("render npd service template: %w", err)
	}

	current, err := os.ReadFile(servicePath) //nolint:gosec // path is constructed, not user input
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

	if err := utilio.InstallFile(servicePath, &buf, 0o644); err != nil { //nolint:gosec // service files must be world-readable
		return false, err
	}
	return true, nil
}

func (t *startTask) ensureSystemdUnit(ctx context.Context, restart bool) error {
	_, err := t.systemd.GetUnitStatus(ctx, systemdUnitNPD)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		if err := t.systemd.DaemonReload(ctx); err != nil {
			return err
		}
		return t.systemd.StartUnit(ctx, systemdUnitNPD)
	case err != nil:
		return err
	default:
		if restart {
			return t.systemd.ReloadOrRestartUnit(ctx, systemdUnitNPD)
		}
		return nil
	}
}
