package linux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"

	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	v20260301 "go.goms.io/aks/AKSFlexNode/components/linux/v20260301"
	"go.goms.io/aks/AKSFlexNode/components/services/actions"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilio"
	"go.goms.io/aks/AKSFlexNode/pkg/utils/utilpb"
)

type configureBaseOSAction struct{}

func newConfigureBaseOSAction() (actions.Server, error) {
	return &configureBaseOSAction{}, nil
}

var _ actions.Server = (*configureBaseOSAction)(nil)

func (a *configureBaseOSAction) ApplyAction(
	ctx context.Context,
	req *actions.ApplyActionRequest,
) (*actions.ApplyActionResponse, error) {
	config, err := utilpb.AnyTo[*v20260301.ConfigureBaseOS](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := a.ensureSysctlConfig(); err != nil {
		return nil, err
	}

	// TODO: configure swap

	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

// TODO: this should be merged with the input request
const sysctlSettings = `
# Kubernetes sysctl settings
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1
`

const sysctlConfigPath = "/etc/sysctl.d/999-sysctl-aks.conf"

func (a *configureBaseOSAction) ensureSysctlConfig() error {
	expectedConfig := []byte(strings.TrimSpace(sysctlSettings))

	currentConfig, err := os.ReadFile(sysctlConfigPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// file does not exist, will create later
	case err != nil:
		return err
	default:
		if bytes.Equal(currentConfig, expectedConfig) {
			// config is already applied, no need to do anything
			return nil
		}
	}

	if err := utilio.WriteFile(sysctlConfigPath, expectedConfig, 0644); err != nil {
		return err
	}
	if err := utilexec.New().Command("sysctl", "--system").Run(); err != nil {
		return err
	}

	return nil
}
