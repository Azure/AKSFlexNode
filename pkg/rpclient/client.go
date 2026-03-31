// Package rpclient provides a lightweight REST client for the AKS RP FlexNode machine APIs.
package rpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/sirupsen/logrus"
)

const (
	apiVersion    = "2026-03-02-preview"
	armScope      = "https://management.azure.com/.default"
	agentPoolName = "flexnode"
)

// Client talks to the AKS RP FlexNode machine APIs.
type Client struct {
	// baseURL is the RP endpoint root including any proxy prefix.
	// For production: "https://management.azure.com"
	// For standalone: "http://localhost:PORT/api/v1/namespaces/rp-ingress/services/rp-ingress-ingress-nginx-controller:80/proxy"
	baseURL    string
	cred       azcore.TokenCredential
	httpClient *http.Client
	logger     *logrus.Logger

	subscriptionID string
	resourceGroup  string
	clusterName    string
	machineName    string
}

// NewClient creates a new RP client for production (management.azure.com).
func NewClient(cred azcore.TokenCredential, logger *logrus.Logger, subscriptionID, resourceGroup, clusterName, machineName string) *Client {
	return &Client{
		baseURL:        "https://management.azure.com",
		cred:           cred,
		httpClient:     http.DefaultClient,
		logger:         logger,
		subscriptionID: subscriptionID,
		resourceGroup:  resourceGroup,
		clusterName:    clusterName,
		machineName:    machineName,
	}
}

// NewStandaloneClient creates a client that talks to a standalone RP through a port-forward.
// baseURL is the direct RP endpoint (e.g., "http://localhost:18081").
// No auth is needed when port-forwarding directly to the RP pod.
func NewStandaloneClient(logger *logrus.Logger, baseURL, subscriptionID, resourceGroup, clusterName, machineName string) *Client {
	return &Client{
		baseURL:        baseURL,
		cred:           nil, // no auth through port-forward
		httpClient: &http.Client{
			Transport: &http.Transport{
				// Disable keep-alive to avoid breaking kubectl port-forward
				DisableKeepAlives: true,
			},
		},
		logger:         logger,
		subscriptionID: subscriptionID,
		resourceGroup:  resourceGroup,
		clusterName:    clusterName,
		machineName:    machineName,
	}
}

// rpPath returns the RP resource path for the machine.
func (c *Client) rpPath() string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s/agentPools/%s/machines/%s",
		c.subscriptionID, c.resourceGroup, c.clusterName, agentPoolName, c.machineName)
}

// machineURL returns the full URL for PUT/GET on the machine resource.
func (c *Client) machineURL() string {
	return c.baseURL + c.rpPath() + "?api-version=" + apiVersion
}

// bootstrapDataURL returns the full URL for the getBootstrapData action.
func (c *Client) bootstrapDataURL() string {
	return c.baseURL + c.rpPath() + "/getBootstrapData?api-version=" + apiVersion
}

// getToken acquires a bearer token for ARM.
func (c *Client) getToken(ctx context.Context) (string, error) {
	if c.cred == nil {
		return "", nil // no auth (e.g., through proxy that handles auth)
	}
	tk, err := c.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armScope}})
	if err != nil {
		return "", fmt.Errorf("get ARM token: %w", err)
	}
	return tk.Token, nil
}

// doRequest executes an HTTP request with auth and returns the body.
func (c *Client) doRequest(ctx context.Context, method, url string, body interface{}) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	token, err := c.getToken(ctx)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("http %s: %w", method, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}

// RegisterMachine calls PUT machine with minimal properties.
// The RP auto-creates the flexnode agent pool if needed.
func (c *Client) RegisterMachine(ctx context.Context, metadata map[string]string) error {
	c.logger.Infof("Registering machine %s with RP", c.machineName)

	props := map[string]interface{}{}
	if len(metadata) > 0 {
		props["metadata"] = metadata
	}
	payload := map[string]interface{}{"properties": props}

	body, status, err := c.doRequest(ctx, http.MethodPut, c.machineURL(), payload)
	if err != nil {
		return fmt.Errorf("register machine: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("register machine: HTTP %d: %s", status, string(body))
	}

	c.logger.Infof("Machine registered (HTTP %d)", status)
	return nil
}

// BootstrapData is the response from getBootstrapData.
type BootstrapData struct {
	KubernetesVersion    string             `json:"kubernetesVersion"`
	ClusterFQDN          string             `json:"clusterFQDN"`
	CACertData           string             `json:"caCertData"`
	BootstrapToken       string             `json:"bootstrapToken"`
	BootstrapTokenExpiry string             `json:"bootstrapTokenExpiry"`
	PodCIDR              string             `json:"podCIDR"`
	ClusterCIDR          string             `json:"clusterCIDR"`
	ServiceCIDR          string             `json:"serviceCIDR"`
	ClusterDNS           string             `json:"clusterDNS"`
	Binaries             *BootstrapBinaries `json:"binaries,omitempty"`
	CNI                  *BootstrapCNI      `json:"cni,omitempty"`
	Node                 *BootstrapNode     `json:"node,omitempty"`
	Images               *BootstrapImages   `json:"images,omitempty"`
	ExtensionData        map[string]string  `json:"extensionData,omitempty"`
}

type BootstrapBinaries struct {
	Kubelet    *BinaryInfo `json:"kubelet,omitempty"`
	Containerd *BinaryInfo `json:"containerd,omitempty"`
	Runc       *BinaryInfo `json:"runc,omitempty"`
}

type BinaryInfo struct {
	URL     string `json:"url"`
	Version string `json:"version"`
}

type BootstrapCNI struct {
	Plugin         string `json:"plugin"`
	Version        string `json:"version"`
	BinaryURL      string `json:"binaryUrl"`
	ConfigTemplate string `json:"configTemplate"`
}

type BootstrapNode struct {
	MaxPods       *int              `json:"maxPods,omitempty"`
	KubeletConfig map[string]string `json:"kubeletConfig,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Taints        []string          `json:"taints,omitempty"`
}

type BootstrapImages struct {
	Pause string `json:"pause"`
}

// GetBootstrapData calls POST getBootstrapData and returns the parsed response.
func (c *Client) GetBootstrapData(ctx context.Context) (*BootstrapData, error) {
	c.logger.Info("Fetching bootstrap data from RP")

	body, status, err := c.doRequest(ctx, http.MethodPost, c.bootstrapDataURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("get bootstrap data: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("get bootstrap data: HTTP %d: %s", status, string(body))
	}

	var data BootstrapData
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, fmt.Errorf("parse bootstrap data: %w", err)
	}

	c.logger.Infof("Bootstrap data: k8s=%s fqdn=%s", data.KubernetesVersion, data.ClusterFQDN)
	return &data, nil
}

// ReportStatus calls PUT machine with status.extensionData.
func (c *Client) ReportStatus(ctx context.Context, extensionData map[string]string) error {
	payload := map[string]interface{}{
		"properties": map[string]interface{}{
			"status": map[string]interface{}{
				"extensionData": extensionData,
			},
		},
	}

	body, status, err := c.doRequest(ctx, http.MethodPut, c.machineURL(), payload)
	if err != nil {
		return fmt.Errorf("report status: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("report status: HTTP %d: %s", status, string(body))
	}

	c.logger.Debug("Status reported to RP")
	return nil
}
