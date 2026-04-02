package v20260301

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"text/template"

	"google.golang.org/protobuf/types/known/anypb"

	"github.com/Azure/AKSFlexNode/components/api"
	"github.com/Azure/AKSFlexNode/components/kubelet"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/systemd"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const apiServerClientCAPath = "/etc/kubernetes/pki/apiserver-client-ca.crt"

type startKubeletServiceAction struct {
	systemd systemd.Manager
}

func newStartKubeletServiceAction() (actions.Server, error) {
	systemdManager := systemd.New()

	return &startKubeletServiceAction{
		systemd: systemdManager,
	}, nil
}

var _ actions.Server = (*startKubeletServiceAction)(nil)

func (s *startKubeletServiceAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	settings, err := utilpb.AnyTo[*kubelet.StartKubeletService](req.GetItem())
	if err != nil {
		return nil, err
	}

	spec, err := api.DefaultAndValidate(settings.GetSpec())
	if err != nil {
		return nil, err
	}

	if err := s.systemPreflightCheck(); err != nil {
		return nil, err
	}

	if err := s.ensureRuntimeFolders(); err != nil {
		return nil, err
	}

	kubeletConfigUpdated, err := s.ensureKubeletConfig(spec)
	if err != nil {
		return nil, err
	}

	needsRestart := kubeletConfigUpdated // if kubelet config is updated, we need to restart the service to pick up the new config
	if err := s.ensureSystemdUnit(ctx, needsRestart, spec); err != nil {
		return nil, err
	}

	item, err := anypb.New(settings)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

var requiredBinaries = []string{
	"jq",
	"iptables",
	"ebtables",
}

func (s *startKubeletServiceAction) systemPreflightCheck() error {
	for _, binary := range requiredBinaries {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("lookup %q: %w", binary, err)
		}
	}

	return nil
}

// ensureRuntimeFolders creates directories that must exist before kubelet
// starts but are not managed by kubelet itself.
func (s *startKubeletServiceAction) ensureRuntimeFolders() error {
	// create static pod dir -- kubelet expects this directory to exist for --pod-manifest-path
	// ref: https://github.com/kubernetes/kubernetes/blob/147a9ee31545188a1a53ad64ed12add16e95f04a/cmd/kubeadm/app/util/staticpod/utils.go#L191-L192
	if err := os.MkdirAll(config.KubeletStaticPodPath, 0700); err != nil {
		return fmt.Errorf("create runtime folder %q: %w", config.KubeletStaticPodPath, err)
	}
	return nil
}
