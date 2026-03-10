package v20260301

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"os"
	"path/filepath"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/api"
	"github.com/Azure/AKSFlexNode/components/cri"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	nvidiaRuntimeDropInName = "99-nvidia-runtime.toml"
)

type startContainerdServiceAction struct {
	systemd systemd.Manager
}

func newStartContainerdServiceAction() (actions.Server, error) {
	systemdManager := systemd.New()

	return &startContainerdServiceAction{
		systemd: systemdManager,
	}, nil
}

var _ actions.Server = (*startContainerdServiceAction)(nil)

func (s *startContainerdServiceAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*cri.StartContainerdService](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(config.GetSpec())
	if err != nil {
		return nil, err
	}

	configUpdated, err := s.ensureContainerdConfig(spec)
	if err != nil {
		return nil, err
	}

	gpuUpdated, err := s.ensureGPUDropInConfigs(spec)
	if err != nil {
		return nil, err
	}

	needsRestart := configUpdated || gpuUpdated
	if err := s.ensureSystemdUnit(ctx, needsRestart); err != nil {
		return nil, err
	}

	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

func (s *startContainerdServiceAction) ensureContainerdConfig(
	spec *cri.StartContainerdServiceSpec,
) (updated bool, err error) {
	expectedConfig := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(expectedConfig, "containerd.toml", map[string]any{
		"SandboxImage":   spec.GetSandboxImage(),
		"RuncBinaryPath": runcBinPath,
		"CNIBinDir":      spec.GetCniConfig().GetBinDir(),
		"CNIConfDir":     spec.GetCniConfig().GetConfigDir(),
		"MetricsAddress": spec.GetMetricsAddress(),
	}); err != nil {
		return false, err
	}

	currentConfig, err := os.ReadFile(config.ContainerdConfigPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Config file doesn't exist, fall through to create new config file
	case err != nil:
		return false, err
	default:
		// Config file exists, compare with expected content
		if bytes.Equal(bytes.TrimSpace(currentConfig), bytes.TrimSpace(expectedConfig.Bytes())) {
			// Config is up-to-date, no update needed
			return false, nil
		}
	}

	if err := utilio.InstallFile(config.ContainerdConfigPath, expectedConfig, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func (s *startContainerdServiceAction) ensureSystemdUnit(ctx context.Context, restart bool) error {
	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, "containerd.service", map[string]any{
		"ContainerdBinPath": containerdBinPath,
	}); err != nil {
		return err
	}

	unitUpdated, err := s.systemd.EnsureUnitFile(ctx, config.SystemdUnitContainerd, b.Bytes())
	if err != nil {
		return err
	}

	return systemd.EnsureUnitRunning(ctx, s.systemd, config.SystemdUnitContainerd, unitUpdated, restart || unitUpdated)
}

// ensureGPUDropInConfigs manages GPU-related containerd drop-in configs.
// Based on the GPUConfig oneof, it ensures the correct drop-in is present.
// When no GPU config is set, the drop-in is removed.
func (s *startContainerdServiceAction) ensureGPUDropInConfigs(
	spec *cri.StartContainerdServiceSpec,
) (updated bool, err error) {
	gpuConfig := spec.GetGpuConfig()

	runtimeUpdated, err := s.ensureDropInConfig(
		nvidiaRuntimeDropInName,
		gpuConfig.GetNvidiaRuntime() != nil,
		map[string]any{
			"RuntimePath":                gpuConfig.GetNvidiaRuntime().GetRuntimePath(),
			"RuntimeClassName":           gpuConfig.GetNvidiaRuntime().GetRuntimeClassName(),
			"DisableSetAsDefaultRuntime": gpuConfig.GetNvidiaRuntime().GetDisableSetAsDefaultRuntime(),
		},
	)
	if err != nil {
		return false, err
	}

	return runtimeUpdated, nil
}

// ensureDropInConfig writes or removes a containerd drop-in config file.
// If enabled is true, the template is rendered and written idempotently.
// If enabled is false, the drop-in is removed if it exists.
func (s *startContainerdServiceAction) ensureDropInConfig(
	dropInName string,
	enabled bool,
	templateData map[string]any,
) (updated bool, err error) {
	dropInPath := filepath.Join(config.ContainerdConfDropInDir, dropInName)

	if !enabled {
		err := os.Remove(dropInPath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return false, nil
		case err != nil:
			return false, err
		default:
			return true, nil
		}
	}

	expectedConfig := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(expectedConfig, dropInName, templateData); err != nil {
		return false, err
	}

	currentConfig, err := os.ReadFile(dropInPath) //#nosec - trusted path constructed from constant and validated input
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Drop-in doesn't exist, fall through to create it
	case err != nil:
		return false, err
	default:
		if bytes.Equal(bytes.TrimSpace(currentConfig), bytes.TrimSpace(expectedConfig.Bytes())) {
			return false, nil
		}
	}

	if err := utilio.InstallFile(dropInPath, expectedConfig, 0644); err != nil {
		return false, err
	}
	return true, nil
}
