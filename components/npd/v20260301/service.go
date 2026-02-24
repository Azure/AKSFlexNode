package v20260301

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"os"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/npd"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/systemd"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	systemdUnitNPD = "node-problem-detector.service"
	npdServicePath = "/etc/systemd/system/node-problem-detector.service"
)

type startNodeProblemDetectorAction struct {
	systemd systemd.Manager
}

func newStartNodeProblemDetectorAction() (actions.Server, error) {
	return &startNodeProblemDetectorAction{
		systemd: systemd.New(),
	}, nil
}

var _ actions.Server = (*startNodeProblemDetectorAction)(nil)

func (s *startNodeProblemDetectorAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*npd.StartNodeProblemDetector](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(settings.GetSpec())
	if err != nil {
		return nil, err
	}

	serviceUpdated, err := s.ensureServiceFile(spec)
	if err != nil {
		return nil, err
	}

	needsRestart := serviceUpdated
	if err := s.ensureSystemdUnit(ctx, needsRestart); err != nil {
		return nil, err
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensureServiceFile writes the NPD systemd service file if it needs updating.
func (s *startNodeProblemDetectorAction) ensureServiceFile(spec *npd.StartNodeProblemDetectorSpec) (updated bool, err error) {
	expectedContent := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(expectedContent, "node-problem-detector.service", map[string]any{
		"NPDBinaryPath":  npdBinaryPath,
		"APIServerURL":   spec.GetApiServer(),
		"KubeconfigPath": spec.GetKubeConfigPath(),
		"NPDConfigPath":  npdConfigPath,
	}); err != nil {
		return false, err
	}

	currentContent, err := os.ReadFile(npdServicePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// Service file doesn't exist, fall through to create it
	case err != nil:
		return false, err
	default:
		if bytes.Equal(bytes.TrimSpace(currentContent), bytes.TrimSpace(expectedContent.Bytes())) {
			return false, nil
		}
	}

	if err := utilio.InstallFile(npdServicePath, expectedContent, 0644); err != nil {
		return false, err
	}
	return true, nil
}

func (s *startNodeProblemDetectorAction) ensureSystemdUnit(ctx context.Context, restart bool) error {
	_, err := s.systemd.GetUnitStatus(ctx, systemdUnitNPD)
	switch {
	case errors.Is(err, systemd.ErrUnitNotFound):
		return s.createSystemdUnit(ctx)
	case err != nil:
		return err
	default:
		return s.updateSystemdUnit(ctx, restart)
	}
}

func (s *startNodeProblemDetectorAction) createSystemdUnit(ctx context.Context) error {
	if err := s.systemd.DaemonReload(ctx); err != nil {
		return err
	}

	if err := s.systemd.StartUnit(ctx, systemdUnitNPD); err != nil {
		return err
	}

	return nil
}

func (s *startNodeProblemDetectorAction) updateSystemdUnit(ctx context.Context, restart bool) error {
	// TODO: should we allow updating npd.service?

	if restart {
		if err := s.systemd.ReloadOrRestartUnit(ctx, systemdUnitNPD); err != nil {
			return err
		}
	}

	return nil
}
