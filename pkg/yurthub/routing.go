package yurthub

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	directKubeconfig = "var/lib/kubelet/kubeconfig"
	proxyKubeconfig  = "var/lib/yurthub/kubelet-proxy.conf"
	routingDropIn    = "etc/systemd/system/kubelet.service.d/99-aks-flex-yurthub.conf"
	proxyURL         = "http://127.0.0.1:10261"

	directKubeconfigArg = "--kubeconfig=/" + directKubeconfig
	proxyKubeconfigArg  = "--kubeconfig=/" + proxyKubeconfig
	// Kept from unbounded's 10-kubeconfig.conf for certificate joins (the only
	// joins Hub supports), so kubelet can still re-bootstrap an expired client
	// certificate. Re-bootstrap rewrites the proxy kubeconfig to the direct API
	// server, so kubelet bypasses Hub until the next enable reroutes it.
	bootstrapKubeconfigArg = "--bootstrap-kubeconfig=/var/lib/kubelet/bootstrap-kubeconfig"
)

var routingUnit = []byte(`[Unit]
Wants=aks-flex-yurthub.service
After=aks-flex-yurthub.service
[Service]
Environment="KUBELET_KUBECONFIG_ARGS=` + bootstrapKubeconfigArg + ` ` + proxyKubeconfigArg + `"
`)

var directRoutingUnit = []byte(`[Service]
Environment="KUBELET_KUBECONFIG_ARGS=` + bootstrapKubeconfigArg + ` ` + directKubeconfigArg + `"
`)

// configureRouting writes the proxy kubeconfig, then the kubelet drop-in that
// selects it. The direct kubeconfig stays for certificate operations and disable.
// It doesn't restart kubelet.
func configureRouting(view isolation.NodeRoot, kubeletUnit string) error {
	root, err := os.OpenRoot(view.Path("/"))
	if err != nil {
		return fmt.Errorf("open worker for hub routing: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	dropIn := routingDropInFor(kubeletUnit)
	if err := checkRoutingOwnership(root, dropIn); err != nil {
		return err
	}
	data, err := readDirectKubeconfig(root, view)
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
	if err := utilio.WriteFileInRoot(root, dropIn, routingUnit, 0o644); err != nil {
		return fmt.Errorf("write hub kubelet drop-in: %w", err)
	}
	return nil
}

// restoreRouting removes the agent's kubelet drop-in and reports whether it was
// present. It doesn't restart kubelet.
func restoreRouting(view isolation.NodeRoot, kubeletUnit string) (bool, error) {
	root, err := os.OpenRoot(view.Path("/"))
	if err != nil {
		return false, fmt.Errorf("open worker for direct routing: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	return restoreRoutingInRoot(root, kubeletUnit)
}

func restoreRoutingInRoot(root *os.Root, kubeletUnit string) (bool, error) {
	dropIn := routingDropInFor(kubeletUnit)
	if err := checkRoutingOwnership(root, dropIn); err != nil {
		return false, err
	}
	err := root.Remove(dropIn)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove hub kubelet drop-in: %w", err)
	}
	return true, nil
}

func validKubeletUnit(unit string) bool {
	return unit != "" && filepath.Base(unit) == unit && !strings.ContainsAny(unit, " \t\r\n")
}

func routingDropInFor(unit string) string {
	return filepath.Join("etc/systemd/system", unit+".d", "99-aks-flex-yurthub.conf")
}

func readDirectKubeconfig(root *os.Root, view isolation.NodeRoot) ([]byte, error) {
	mapped := view.Path("/" + directKubeconfig)
	if mapped == filepath.Join(view.Path("/"), directKubeconfig) {
		return root.ReadFile(directKubeconfig)
	}
	kubelet, err := os.OpenRoot(view.Path("/var/lib/kubelet"))
	if err != nil {
		return nil, err
	}
	defer kubelet.Close() //nolint:errcheck // directory handle
	return kubelet.ReadFile("kubeconfig")
}

func checkRoutingOwnership(root *os.Root, dropIn string) error {
	info, err := root.Lstat(dropIn)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect hub routing override: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("hub routing override is not a regular file")
	}
	data, err := root.ReadFile(dropIn)
	if err != nil {
		return fmt.Errorf("read hub routing override: %w", err)
	}
	if !bytes.Equal(data, routingUnit) && !bytes.Equal(data, directRoutingUnit) {
		return fmt.Errorf("hub routing override has conflicting contents")
	}
	return nil
}
