package daemon

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const kubeletDefaultsPath = "etc/default/kubelet"

type configureKubeletDefaultsTask struct {
	cfg        config.KubeletConfig
	machineDir string
}

func ConfigureKubeletDefaults(cfg *config.Config, machineDir string) phases.Task {
	return &configureKubeletDefaultsTask{
		cfg:        cfg.Node.Kubelet,
		machineDir: machineDir,
	}
}

func (t *configureKubeletDefaultsTask) Name() string { return "configure-kubelet-defaults" }

func (t *configureKubeletDefaultsTask) Do(context.Context) error {
	content, err := renderKubeletDefaults(t.cfg)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return nil
	}

	path := filepath.Join(t.machineDir, kubeletDefaultsPath)
	if err := utilio.WriteFile(path, content, 0o644); err != nil {
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
		"# Managed by aks-flex-node. Do not edit directly.\nKUBELET_EXTRA_ARGS=--node-ip=%s\n",
		addr.String(),
	)), nil
}
