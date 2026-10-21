package yurthub

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	directKubeconfig = "var/lib/kubelet/kubeconfig"
	proxyKubeconfig  = "var/lib/yurthub/kubelet-proxy.conf"
	routingDropIn    = "etc/systemd/system/kubelet.service.d/99-aks-flex-yurthub.conf"
	proxyURL         = "http://127.0.0.1:10261"

	directKubeconfigArg = "--kubeconfig=/" + directKubeconfig
	proxyKubeconfigArg  = "--kubeconfig=/" + proxyKubeconfig
)

var routingUnit = []byte(`[Unit]
Wants=aks-flex-yurthub.service
After=aks-flex-yurthub.service
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=` + proxyKubeconfigArg + `"
`)

// configureRouting writes the proxy kubeconfig, then the kubelet drop-in that
// selects it. The direct kubeconfig stays for certificate operations and disable.
// It doesn't restart kubelet.
func configureRouting(machineDir string) error {
	root, err := os.OpenRoot(machineDir)
	if err != nil {
		return fmt.Errorf("open worker for hub routing: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	if err := checkRoutingOwnership(root); err != nil {
		return err
	}
	data, err := root.ReadFile(directKubeconfig)
	if err != nil {
		return fmt.Errorf("read direct kubelet config: %w", err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		// Parser errors may include credential-bearing configuration fragments.
		return fmt.Errorf("decode direct kubelet config: invalid kubeconfig")
	}
	context := cfg.Contexts[cfg.CurrentContext]
	if context == nil || cfg.Clusters[context.Cluster] == nil {
		return fmt.Errorf("direct kubelet config has no current cluster")
	}
	cluster := cfg.Clusters[context.Cluster]
	upstream, err := url.Parse(cluster.Server)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" || upstream.User != nil {
		return fmt.Errorf("direct kubelet config must reference an HTTPS upstream")
	}
	// An explicit proxy would also apply to the loopback Hub hop, and Hub has no
	// defined upstream proxy support. Fail activation rather than guess. Omit the
	// value from the error because proxy URLs can contain credentials.
	if cluster.ProxyURL != "" {
		return fmt.Errorf("direct kubelet config uses an explicit proxy; hub routing does not support proxies")
	}
	// Moving a kubeconfig changes how client-go resolves relative certificate
	// and key paths. Refuse that ambiguity rather than silently break recovery.
	for _, path := range clientcmd.GetConfigFileReferences(cfg) {
		if path != nil && *path != "" && (*path)[0] != '/' {
			return fmt.Errorf("direct kubelet config has relative credential paths")
		}
	}
	cluster.Server = proxyURL
	data, err = clientcmd.Write(*cfg)
	if err != nil {
		return fmt.Errorf("encode proxy kubelet config")
	}
	if err := utilio.WriteFileInRoot(root, proxyKubeconfig, data, 0o600); err != nil {
		return fmt.Errorf("write proxy kubelet config: %w", err)
	}
	if err := utilio.WriteFileInRoot(root, routingDropIn, routingUnit, 0o644); err != nil {
		return fmt.Errorf("write hub kubelet drop-in: %w", err)
	}
	return nil
}

// restoreRouting removes the agent's kubelet drop-in and reports whether it was
// present. It doesn't restart kubelet.
func restoreRouting(machineDir string) (bool, error) {
	root, err := os.OpenRoot(machineDir)
	if err != nil {
		return false, fmt.Errorf("open worker for direct routing: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	if err := checkRoutingOwnership(root); err != nil {
		return false, err
	}
	err = root.Remove(routingDropIn)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove hub kubelet drop-in: %w", err)
	}
	return true, nil
}

func checkRoutingOwnership(root *os.Root) error {
	info, err := root.Lstat(routingDropIn)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect hub routing override: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hub routing override is not a regular file")
	}
	data, err := root.ReadFile(routingDropIn)
	if err != nil {
		return fmt.Errorf("read hub routing override: %w", err)
	}
	if !bytes.Equal(data, routingUnit) {
		return fmt.Errorf("hub routing override has conflicting contents")
	}
	return nil
}
