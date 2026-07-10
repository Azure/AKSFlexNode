package aksmachine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"k8s.io/client-go/rest"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/kubeauth"
)

const maxEndpointErrorBody = 4096

type clusterEndpointClient struct {
	httpClient *http.Client
	baseURL    *url.URL
	nodeName   string
	logger     *slog.Logger
}

type machineStatusPatch struct {
	Properties struct {
		Status Status `json:"status"`
	} `json:"properties"`
}

type kubernetesStatusError struct {
	Kind    string `json:"kind,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Details *struct {
		Kind string `json:"kind,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"details,omitempty"`
}

// newClusterEndpointClientFromBootstrapConfig is used by start for EnsureMachine
// before daemon credentials exist. EnsureMachine is a no-op when the controller
// has already created the machine.
func newClusterEndpointClientFromBootstrapConfig(cfg *config.Config, logger *slog.Logger) (MachineClient, error) {
	restCfg, err := kubeauth.BootstrapRESTConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newClusterEndpointClient(cfg, logger, restCfg)
}

func newClusterEndpointClient(cfg *config.Config, logger *slog.Logger, restCfg *rest.Config) (*clusterEndpointClient, error) {
	if restCfg == nil {
		return nil, fmt.Errorf("kubernetes REST config is nil")
	}
	if cfg.Agent.NodeName == "" {
		return nil, fmt.Errorf("node name is empty")
	}
	baseURL, err := clusterEndpointBaseURL(restCfg, cfg.Agent.MachineClient.EndpointURL)
	if err != nil {
		return nil, err
	}
	httpClient, err := rest.HTTPClientFor(restCfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes HTTP client for machine endpoint: %w", err)
	}
	return &clusterEndpointClient{
		httpClient: httpClient,
		baseURL:    baseURL,
		nodeName:   cfg.Agent.NodeName,
		logger:     logger,
	}, nil
}

func clusterEndpointBaseURL(restCfg *rest.Config, endpointURL string) (*url.URL, error) {
	endpointURL = strings.TrimSpace(endpointURL)
	if endpointURL == "" {
		return nil, fmt.Errorf("in-cluster machine endpoint URL is empty")
	}
	parsedEndpoint, err := url.Parse(endpointURL)
	if err != nil {
		return nil, fmt.Errorf("parse in-cluster machine endpoint URL: %w", err)
	}
	if parsedEndpoint.Scheme != "" {
		if parsedEndpoint.Host == "" {
			return nil, fmt.Errorf("in-cluster machine endpoint URL host is empty")
		}
		return parsedEndpoint, nil
	}
	if !strings.HasPrefix(endpointURL, "/") {
		return nil, fmt.Errorf("in-cluster machine endpoint URL must be absolute or start with /")
	}
	if restCfg.Host == "" {
		return nil, fmt.Errorf("kubernetes REST config host is empty")
	}
	host, err := url.Parse(strings.TrimRight(restCfg.Host, "/"))
	if err != nil || host.Scheme == "" || host.Host == "" {
		return nil, fmt.Errorf("invalid Kubernetes REST config host %q", restCfg.Host)
	}
	base := *host
	base.Path = path.Clean(endpointURL)
	base.RawPath = ""
	base.RawQuery = parsedEndpoint.RawQuery
	return &base, nil
}

func (c *clusterEndpointClient) Create(ctx context.Context, desired GoalState) (*Machine, error) {
	requestURL := c.machineURL(c.nodeName)
	payload := armcontainerservice.Machine{
		Properties: &armcontainerservice.MachineProperties{
			Kubernetes: buildK8sProfile(desired),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal cluster endpoint machine create request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create cluster endpoint machine create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create machine through cluster endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNoContent {
		c.logger.Debug("cluster endpoint did not apply machine create request; verifying pre-created machine", "status", resp.Status)
		return c.adoptExistingMachine(ctx, desired)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, clusterEndpointHTTPError("create machine through cluster endpoint", requestURL, resp)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read cluster endpoint machine create response: %w", err)
	}
	if strings.TrimSpace(string(data)) == "" {
		return c.adoptExistingMachine(ctx, desired)
	}
	machine, err := machineFromEndpointJSON(data)
	if err != nil {
		return nil, err
	}
	if machine.ID == "" {
		machine.ID = requestURL.String()
	}
	if machine.Name == "" {
		machine.Name = c.nodeName
	}
	if err := validateAdoptedMachine(machine, desired); err != nil {
		return nil, err
	}
	return machine, nil
}

func (c *clusterEndpointClient) adoptExistingMachine(ctx context.Context, desired GoalState) (*Machine, error) {
	machine, err := c.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify pre-created machine from cluster endpoint: %w", err)
	}
	if err := validateAdoptedMachine(machine, desired); err != nil {
		return nil, err
	}
	return machine, nil
}

func validateAdoptedMachine(machine *Machine, desired GoalState) error {
	if machine == nil {
		return fmt.Errorf("cluster endpoint returned nil machine")
	}
	if desired.KubernetesVersion != "" && machine.Goal.KubernetesVersion != "" && machine.Goal.KubernetesVersion != desired.KubernetesVersion {
		return fmt.Errorf("pre-created machine Kubernetes version %q does not match desired %q", machine.Goal.KubernetesVersion, desired.KubernetesVersion)
	}
	return nil
}

func (c *clusterEndpointClient) Get(ctx context.Context) (*Machine, error) {
	requestURL := c.machineURL(c.nodeName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create cluster endpoint machine request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get machine from cluster endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, clusterEndpointHTTPError("get machine from cluster endpoint", requestURL, resp)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read cluster endpoint machine response: %w", err)
	}
	machine, err := machineFromEndpointJSON(data)
	if err != nil {
		return nil, err
	}
	if machine.ID == "" {
		machine.ID = requestURL.String()
	}
	if machine.Name == "" {
		machine.Name = c.nodeName
	}
	return machine, nil
}

func (c *clusterEndpointClient) PatchStatus(ctx context.Context, status Status) error {
	requestURL := c.machineStatusURL(c.nodeName)
	payload := machineStatusPatch{}
	payload.Properties.Status = status
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal cluster endpoint machine status request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, requestURL.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create cluster endpoint machine status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("patch machine status through cluster endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusMethodNotAllowed {
		c.logger.Debug("cluster endpoint rejected machine status mutation", "status", resp.Status)
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return clusterEndpointHTTPError("patch machine status through cluster endpoint", requestURL, resp)
	}
	return nil
}

func (c *clusterEndpointClient) machineURL(machineName string) *url.URL {
	u := *c.baseURL
	u.Path = joinEndpointURLPath(u.Path, "machines", machineName)
	u.RawPath = ""
	return &u
}

func (c *clusterEndpointClient) machineStatusURL(machineName string) *url.URL {
	u := *c.baseURL
	u.Path = joinEndpointURLPath(u.Path, "machines", machineName, "status")
	u.RawPath = ""
	return &u
}

func clusterEndpointHTTPError(operation string, requestURL *url.URL, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxEndpointErrorBody))
	if resp.StatusCode == http.StatusNotFound {
		if isKubernetesServiceProxyNotFound(body) {
			return fmt.Errorf("%s: status %s: %s", operation, resp.Status, strings.TrimSpace(string(body)))
		}
		return &NotFoundError{Resource: requestURL.String()}
	}
	return fmt.Errorf("%s: status %s: %s", operation, resp.Status, strings.TrimSpace(string(body)))
}

func isKubernetesServiceProxyNotFound(data []byte) bool {
	var status kubernetesStatusError
	if err := json.Unmarshal(data, &status); err != nil {
		return false
	}
	return status.Kind == "Status" && strings.EqualFold(status.Reason, "NotFound")
}

func machineFromEndpointJSON(data []byte) (*Machine, error) {
	var armMachine armcontainerservice.Machine
	if err := json.Unmarshal(data, &armMachine); err != nil {
		return nil, fmt.Errorf("decode cluster endpoint machine response: %w", err)
	}
	return machineFromARM(armMachine, GoalState{}), nil
}

func joinEndpointURLPath(base string, elem ...string) string {
	parts := []string{base}
	parts = append(parts, elem...)
	joined := path.Join(parts...)
	if strings.HasSuffix(elem[len(elem)-1], "/") && !strings.HasSuffix(joined, "/") {
		joined += "/"
	}
	return joined
}

var _ MachineClient = (*clusterEndpointClient)(nil)
