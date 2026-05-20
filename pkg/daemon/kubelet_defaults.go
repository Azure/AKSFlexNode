package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	kubeletDefaultsPath   = "etc/default/kubelet"
	kubeletDefaultsHeader = "# Managed by aks-flex-node. Do not edit directly.\n"
)

type configureKubeletDefaultsTask struct {
	cfg        config.KubeletConfig
	machineDir string
}

// ConfigureKubeletDefaults writes the environment file consumed by unbounded's kubelet unit.
func ConfigureKubeletDefaults(cfg *config.Config, machineDir string) phases.Task {
	return &configureKubeletDefaultsTask{
		cfg:        cfg.Node.Kubelet,
		machineDir: machineDir,
	}
}

func (t *configureKubeletDefaultsTask) Name() string { return "configure-kubelet-defaults" }

func (t *configureKubeletDefaultsTask) Do(context.Context) error {
	path := filepath.Join(t.machineDir, kubeletDefaultsPath)
	content, err := renderKubeletDefaults(t.cfg)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return removeManagedKubeletDefaults(path)
	}

	if err := utilio.WriteFile(path, content, 0o644); err != nil { //nolint:gosec // kubelet defaults contain no secrets and follow /etc/default readability
		return fmt.Errorf("writing kubelet defaults: %w", err)
	}
	return nil
}

func renderKubeletDefaults(cfg config.KubeletConfig) ([]byte, error) {
	if cfg.NodeIP == "" {
		return nil, nil
	}
	addr, err := netip.ParseAddr(cfg.NodeIP)
	if err != nil {
		return nil, fmt.Errorf("node.kubelet.nodeIP %q is not a valid IP address: %w", cfg.NodeIP, err)
	}

	return []byte(fmt.Sprintf(
		kubeletDefaultsHeader+"KUBELET_EXTRA_ARGS=--node-ip=%s\n",
		addr.String(),
	)), nil
}

func removeManagedKubeletDefaults(path string) error {
	existing, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // path is constructed under the machine rootfs
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading kubelet defaults: %w", err)
	}
	if !bytes.HasPrefix(existing, []byte(kubeletDefaultsHeader)) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing kubelet defaults: %w", err)
	}
	return nil
}
