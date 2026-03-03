package v20260301

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	"google.golang.org/protobuf/types/known/anypb"
	utilexec "k8s.io/utils/exec"

	"github.com/Azure/AKSFlexNode/components/linux"
	"github.com/Azure/AKSFlexNode/components/services/actions"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilpb"
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
	config, err := utilpb.AnyTo[*linux.ConfigureBaseOS](req.GetItem())
	if err != nil {
		return nil, err
	}

	if err := a.ensurePackages(ctx); err != nil {
		return nil, err
	}

	if err := a.ensureSysctlConfig(ctx); err != nil {
		return nil, err
	}

	if err := a.disableSwap(ctx); err != nil {
		return nil, err
	}

	item, err := anypb.New(config)
	if err != nil {
		return nil, err
	}

	return actions.ApplyActionResponse_builder{Item: item}.Build(), nil
}

var requiredPackages = []string{
	"iptables",
	"jq",
}

func (a *configureBaseOSAction) hasPackage(pkg string) bool {
	_, err := exec.LookPath(pkg)
	if err == nil {
		// package is already installed
		return true
	}

	return false
}

func (a *configureBaseOSAction) ensurePackages(ctx context.Context) error {
	// TODO: we should use distro-specific methods to detect and install packages.
	packagesToInstall := map[string]struct{}{}
	for _, pkg := range requiredPackages {
		if !a.hasPackage(pkg) {
			packagesToInstall[pkg] = struct{}{}
		}
	}
	if len(packagesToInstall) == 0 {
		return nil
	}

	cmd := utilexec.New()

	if err := cmd.CommandContext(ctx, "apt", "update").Run(); err != nil {
		return err
	}

	args := []string{"install", "-y"}
	for k := range packagesToInstall {
		args = append(args, k)
	}
	if err := cmd.CommandContext(ctx, "apt", args...).Run(); err != nil {
		return err
	}

	return nil
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

func (a *configureBaseOSAction) ensureSysctlConfig(ctx context.Context) error {
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
	if err := utilexec.New().CommandContext(ctx, "sysctl", "--system").Run(); err != nil {
		return err
	}

	return nil
}

const fstabPath = "/etc/fstab"

func (a *configureBaseOSAction) disableSwap(ctx context.Context) error {
	if err := utilexec.New().CommandContext(ctx, "swapoff", "-a").Run(); err != nil {
		return err
	}

	if err := a.commentOutSwapInFstab(fstabPath); err != nil {
		return err
	}

	return nil
}

// commentOutSwapInFstab reads the fstab file at the given path, comments out
// any uncommented lines containing "swap", and writes the result back. A backup
// of the original file is saved to <path>.bak before any modifications are made.
func (a *configureBaseOSAction) commentOutSwapInFstab(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// no fstab, nothing to do
			return nil
		}
		return err
	}

	lines := strings.Split(string(content), "\n")
	modified := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// skip empty lines and already-commented lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "swap") {
			lines[i] = "# " + line
			modified = true
		}
	}

	if !modified {
		return nil
	}

	// back up the original fstab before writing changes
	if err := utilio.WriteFile(path+".bak", content, 0644); err != nil {
		return err
	}

	newContent := []byte(strings.Join(lines, "\n"))
	if err := utilio.WriteFile(path, newContent, 0644); err != nil {
		return err
	}

	return nil
}
