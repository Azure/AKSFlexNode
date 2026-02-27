package v20260301

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"os"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/api"
	"github.com/Azure/AKSFlexNode/components/cri"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	systemdUnitContainerd = "containerd.service"
	containerdConfigPath  = "/etc/containerd/config.toml"
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

	needsRestart := configUpdated // if config is updated, we need to restart containerd to apply new config
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

	currentConfig, err := os.ReadFile(containerdConfigPath)
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

	if err := utilio.InstallFile(containerdConfigPath, expectedConfig, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func (s *startContainerdServiceAction) ensureSystemdUnit(ctx context.Context, restart bool) error {
	_, err := s.systemd.GetUnitStatus(ctx, systemdUnitContainerd)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		return s.createSystemdUnit(ctx)
	case err != nil:
		return err
	default:
		return s.updateSystemdUnit(ctx, restart)
	}
}

func (s *startContainerdServiceAction) createSystemdUnit(ctx context.Context) error {
	b := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(b, "containerd.service", map[string]any{
		"ContainerdBinPath": containerdBinPath,
	}); err != nil {
		return err
	}

	if err := s.systemd.WriteUnitFile(ctx, systemdUnitContainerd, b.Bytes()); err != nil {
		return err
	}

	if err := s.systemd.DaemonReload(ctx); err != nil {
		return err
	}

	if err := s.systemd.StartUnit(ctx, systemdUnitContainerd); err != nil {
		return err
	}

	return nil
}

func (s *startContainerdServiceAction) updateSystemdUnit(ctx context.Context, restart bool) error {
	// TODO: should we allow updating containerd.service?

	if restart {
		if err := s.systemd.ReloadOrRestartUnit(ctx, systemdUnitContainerd); err != nil {
			return err
		}
	}

	return nil
}
