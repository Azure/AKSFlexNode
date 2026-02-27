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

	"go.goms.io/aks/AKSFlexNode/components/api"
	"go.goms.io/aks/AKSFlexNode/components/cni"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/config"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	bridgeConfigFile = "99-bridge.conf"
)

type configureCNIAction struct{}

func newConfigureCNIAction() (actions.Server, error) {
	return &configureCNIAction{}, nil
}

var _ actions.Server = (*configureCNIAction)(nil)

func (a *configureCNIAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*cni.ConfigureCNI](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(settings.GetSpec())
	if err != nil {
		return nil, err
	}

	if err := a.ensureBridgeConfig(spec.GetCniSpecVersion()); err != nil {
		return nil, err
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// ensureBridgeConfig renders the 99-bridge.conf template and installs it to the
// host's CNI config directory. It is idempotent: if the file already exists with
// the expected content, no write is performed.
func (a *configureCNIAction) ensureBridgeConfig(cniVersion string) error {
	expectedConfig := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(expectedConfig, bridgeConfigFile, map[string]any{
		"CniVersion": cniVersion,
	}); err != nil {
		return err
	}

	targetPath := filepath.Join(config.DefaultCNIConfigDir, bridgeConfigFile)

	currentConfig, err := os.ReadFile(targetPath) // #nosec - path has been validated by caller
	switch {
	case errors.Is(err, os.ErrNotExist):
		// file does not exist, fall through to create it
	case err != nil:
		return err
	default:
		if bytes.Equal(bytes.TrimSpace(currentConfig), bytes.TrimSpace(expectedConfig.Bytes())) {
			return nil
		}
	}

	return utilio.InstallFile(targetPath, expectedConfig, 0644)
}
