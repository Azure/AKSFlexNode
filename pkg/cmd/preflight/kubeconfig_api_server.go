package preflight

import (
	"context"
	"net/http"
	"strings"
	"time"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/unbounded/pkg/agent/preflight"
)

const checkAPIServerReachableName = "api-server-reachable"

type kubeconfigAPIServerReachableChecker struct {
	data string
}

func (c kubeconfigAPIServerReachableChecker) Name() string {
	return checkAPIServerReachableName
}

func (c kubeconfigAPIServerReachableChecker) Check(ctx context.Context) []preflight.Result {
	const target = "cluster API server"

	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(c.data))
	if err != nil {
		return preflight.ResultsError(c.Name(), target, "cannot load kubelet kubeconfig: %v", err)
	}
	// Copy only transport settings: the kubelet's exec plugin may exist only
	// inside nspawn and must not run during host-side reachability checks.
	client, err := rest.HTTPClientFor(&rest.Config{
		TLSClientConfig: cfg.TLSClientConfig,
		Proxy:           cfg.Proxy,
		Timeout:         10 * time.Second,
	})
	if err != nil {
		return preflight.ResultsError(c.Name(), target, "cannot build kubelet TLS transport: %v", err)
	}
	defer client.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(cfg.Host, "/")+"/readyz", http.NoBody)
	if err != nil {
		return preflight.ResultsError(c.Name(), target, "cannot create API server request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return preflight.ResultsError(c.Name(), target, "API server is not reachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusInternalServerError {
		return preflight.ResultsError(c.Name(), target, "API server returned status %d", resp.StatusCode)
	}
	return preflight.ResultsOK(c.Name(), target, "API server is reachable")
}
