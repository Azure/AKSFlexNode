package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	defaultAgentPoolName    = "aksflexnodes"
	resourceManagerEndpoint = "https://management.azure.com"
)

type commandRunner interface {
	LookPath(name string) error
	Run(ctx context.Context, name string, args []string, input string) (string, error)
}

type execRunner struct {
	stderr io.Writer
}

func (runner execRunner) LookPath(name string) error {
	_, err := exec.LookPath(name)
	return err
}

func (runner execRunner) Run(ctx context.Context, name string, args []string, input string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- executable names are fixed to az and kubectl
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	cmd.Stderr = runner.stderr
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

type dependencies struct {
	runner commandRunner
	now    func() time.Time
	random io.Reader
	stdout io.Writer
	stderr io.Writer
}

type clusterOptions struct {
	resourceGroup string
	clusterName   string
	subscription  string
}

type generateOptions struct {
	clusterOptions
	agentPoolName    string
	bootstrapToken   bool
	identity         bool
	servicePrincipal bool
	username         string
	password         string
	tenant           string
	arc              bool
	output           string
}

func NewCommand() *cobra.Command {
	deps := dependencies{
		now:    time.Now,
		random: rand.Reader,
		stdout: os.Stdout,
		stderr: os.Stderr,
	}
	deps.runner = execRunner{stderr: deps.stderr}
	return newCommand(deps)
}

func newCommand(deps dependencies) *cobra.Command {
	root := &cobra.Command{
		Use:           "aks-flex-config",
		Short:         "Prepare AKS Flex Node configuration",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetOut(deps.stdout)
	root.SetErr(deps.stderr)

	rbacOptions := clusterOptions{}
	rbac := &cobra.Command{
		Use:   "setup-node-rbac",
		Short: "Apply node bootstrap RBAC bindings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setupNodeRBAC(cmd.Context(), deps, rbacOptions)
		},
	}
	addClusterFlags(rbac, &rbacOptions)

	generateOptions := generateOptions{agentPoolName: defaultAgentPoolName, output: "-"}
	generate := &cobra.Command{
		Use:   "generate-node-config",
		Short: "Render a Flex Node config",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return generateNodeConfig(cmd.Context(), deps, generateOptions)
		},
	}
	addClusterFlags(generate, &generateOptions.clusterOptions)
	generate.Flags().StringVar(&generateOptions.agentPoolName, "agent-pool-name", defaultAgentPoolName, "AKS agent pool name")
	generate.Flags().BoolVar(&generateOptions.bootstrapToken, "bootstrap-token", false, "Use Kubernetes bootstrap token authentication")
	generate.Flags().BoolVar(&generateOptions.identity, "identity", false, "Use managed identity authentication")
	generate.Flags().BoolVar(&generateOptions.servicePrincipal, "service-principal", false, "Use service principal authentication")
	generate.Flags().StringVar(&generateOptions.username, "username", "", "Managed identity client ID or service principal client ID")
	generate.Flags().StringVar(&generateOptions.password, "password", "", "Service principal client secret")
	generate.Flags().StringVar(&generateOptions.tenant, "tenant", "", "Service principal tenant ID")
	generate.Flags().BoolVar(&generateOptions.arc, "arc", false, "Use Azure Arc authentication")
	generate.Flags().StringVar(&generateOptions.output, "output", "-", "Output path, or - for stdout")

	root.AddCommand(rbac, generate)
	return root
}

func addClusterFlags(cmd *cobra.Command, options *clusterOptions) {
	cmd.Flags().StringVar(&options.resourceGroup, "resource-group", "", "Resource group containing the AKS cluster")
	cmd.Flags().StringVar(&options.clusterName, "cluster-name", "", "AKS cluster name")
	cmd.Flags().StringVar(&options.subscription, "subscription", "", "Azure subscription ID or name")
	_ = cmd.MarkFlagRequired("resource-group")
	_ = cmd.MarkFlagRequired("cluster-name")
}

func setupNodeRBAC(ctx context.Context, deps dependencies, options clusterOptions) error {
	if err := requireCommands(deps.runner, "az", "kubectl"); err != nil {
		return err
	}
	if err := loadAdminKubeconfig(ctx, deps, options); err != nil {
		return err
	}
	fmt.Fprintln(deps.stderr, "INFO: applying bootstrap token RBAC bindings")
	if _, err := deps.runner.Run(ctx, "kubectl", []string{"apply", "-f", "-"}, rbacManifest); err != nil {
		return fmt.Errorf("apply bootstrap token RBAC: %w", err)
	}
	return nil
}

func generateNodeConfig(ctx context.Context, deps dependencies, options generateOptions) error {
	mode, err := selectedAuthMode(options)
	if err != nil {
		return err
	}
	commands := []string{"az"}
	if mode == "bootstrap-token" {
		commands = append(commands, "kubectl")
	}
	if err := requireCommands(deps.runner, commands...); err != nil {
		return err
	}

	metadata, err := getClusterMetadata(ctx, deps, options.clusterOptions)
	if err != nil {
		return err
	}
	poolName := strings.TrimSpace(options.agentPoolName)
	if poolName == "" {
		poolName = defaultAgentPoolName
	}
	metadata["agent_pool_name"] = poolName

	config, err := renderConfig(ctx, deps, options, mode, metadata)
	if err != nil {
		return err
	}
	return writeConfig(deps, config, options.output)
}

func selectedAuthMode(options generateOptions) (string, error) {
	modes := []struct {
		name     string
		selected bool
	}{
		{name: "bootstrap-token", selected: options.bootstrapToken},
		{name: "identity", selected: options.identity},
		{name: "service-principal", selected: options.servicePrincipal || options.password != ""},
		{name: "arc", selected: options.arc},
	}
	selected := ""
	for _, mode := range modes {
		if !mode.selected {
			continue
		}
		if selected != "" {
			return "", errors.New("choose exactly one auth mode: --bootstrap-token, --identity, --service-principal, or --arc")
		}
		selected = mode.name
	}
	if selected == "" {
		return "", errors.New("choose exactly one auth mode: --bootstrap-token, --identity, --service-principal, or --arc")
	}
	if selected == "service-principal" && (options.username == "" || options.password == "") {
		return "", errors.New("service principal auth requires --username <client-id> and --password <client-secret>")
	}
	if selected == "arc" {
		return "", errors.New("arc bootstrap data must be fetched on the connected host; use scripts/bootstrap.sh --auth arc --fetch-bootstrap-data")
	}
	return selected, nil
}

func requireCommands(runner commandRunner, names ...string) error {
	for _, name := range names {
		if err := runner.LookPath(name); err != nil {
			return fmt.Errorf("missing required command: %s", name)
		}
	}
	return nil
}

func loadAdminKubeconfig(ctx context.Context, deps dependencies, options clusterOptions) error {
	fmt.Fprintln(deps.stderr, "INFO: loading AKS admin kubeconfig")
	args := append([]string{"aks", "get-credentials"}, aksArgs(options)...)
	args = append(args, "--admin", "--overwrite-existing")
	if _, err := deps.runner.Run(ctx, "az", args, ""); err != nil {
		return fmt.Errorf("load AKS admin kubeconfig: %w", err)
	}
	return nil
}

func aksArgs(options clusterOptions) []string {
	args := []string{"--resource-group", options.resourceGroup, "--name", options.clusterName}
	if options.subscription != "" {
		args = append(args, "--subscription", options.subscription)
	}
	return args
}

func accountArgs(subscription string) []string {
	if subscription == "" {
		return nil
	}
	return []string{"--subscription", subscription}
}

func getClusterMetadata(ctx context.Context, deps dependencies, options clusterOptions) (map[string]string, error) {
	fmt.Fprintln(deps.stderr, "INFO: fetching AKS cluster metadata")
	queries := []struct {
		key  string
		args []string
	}{
		{key: "subscription_id", args: append([]string{"account", "show"}, accountArgs(options.subscription)...)},
		{key: "tenant_id", args: append([]string{"account", "show"}, accountArgs(options.subscription)...)},
		{key: "resource_id", args: append([]string{"aks", "show"}, aksArgs(options)...)},
		{key: "location", args: append([]string{"aks", "show"}, aksArgs(options)...)},
		{key: "kubernetes_version", args: append([]string{"aks", "show"}, aksArgs(options)...)},
		{key: "dns_service_ip", args: append([]string{"aks", "show"}, aksArgs(options)...)},
	}
	queryValues := []string{"id", "tenantId", "id", "location", "currentKubernetesVersion || kubernetesVersion", "networkProfile.dnsServiceIp"}
	metadata := make(map[string]string, len(queries))
	for index, query := range queries {
		args := append(query.args, "--query", queryValues[index], "-o", "tsv")
		value, err := deps.runner.Run(ctx, "az", args, "")
		if err != nil {
			return nil, fmt.Errorf("fetch AKS %s: %w", query.key, err)
		}
		metadata[query.key] = value
	}
	return metadata, nil
}

func renderConfig(ctx context.Context, deps dependencies, options generateOptions, mode string, metadata map[string]string) (map[string]any, error) {
	azure := map[string]any{
		"subscriptionId":          metadata["subscription_id"],
		"tenantId":                metadata["tenant_id"],
		"resourceManagerEndpoint": resourceManagerEndpoint,
		"targetAgentPoolName":     metadata["agent_pool_name"],
		"targetCluster": map[string]any{
			"resourceId": metadata["resource_id"],
			"location":   metadata["location"],
		},
	}
	config := map[string]any{
		"azure":      azure,
		"agent":      map[string]any{"logLevel": "info", "logDir": "/var/log/aks-flex-node"},
		"components": map[string]any{"kubernetes": metadata["kubernetes_version"]},
	}

	switch mode {
	case "bootstrap-token":
		if err := loadAdminKubeconfig(ctx, deps, options.clusterOptions); err != nil {
			return nil, err
		}
		token, err := generateBootstrapToken(ctx, deps)
		if err != nil {
			return nil, err
		}
		serverURL, err := deps.runner.Run(ctx, "kubectl", []string{"config", "view", "--minify", "-o", "jsonpath={.clusters[0].cluster.server}"}, "")
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes API server: %w", err)
		}
		caCertData, err := deps.runner.Run(ctx, "kubectl", []string{"config", "view", "--minify", "--raw", "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}"}, "")
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes CA data: %w", err)
		}
		azure["bootstrapToken"] = map[string]any{"token": token}
		azure["arc"] = map[string]any{"enabled": false}
		config["node"] = map[string]any{"kubelet": map[string]any{"clusterFQDN": clusterFQDN(serverURL), "caCertData": caCertData}}
		if metadata["dns_service_ip"] != "" {
			config["networking"] = map[string]any{"dnsServiceIP": metadata["dns_service_ip"]}
		}
	case "identity":
		identity := map[string]any{}
		if options.username != "" {
			identity["clientId"] = options.username
		}
		azure["managedIdentity"] = identity
		azure["arc"] = map[string]any{"enabled": false}
	case "service-principal":
		tenantID := options.tenant
		if tenantID == "" {
			tenantID = metadata["tenant_id"]
		}
		azure["servicePrincipal"] = map[string]any{"tenantId": tenantID, "clientId": options.username, "clientSecret": options.password}
		azure["arc"] = map[string]any{"enabled": false}
	default:
		return nil, fmt.Errorf("unsupported auth mode: %s", mode)
	}
	return config, nil
}

func generateBootstrapToken(ctx context.Context, deps dependencies) (string, error) {
	tokenID := make([]byte, 3)
	tokenSecret := make([]byte, 8)
	if _, err := io.ReadFull(deps.random, tokenID); err != nil {
		return "", fmt.Errorf("generate bootstrap token ID: %w", err)
	}
	if _, err := io.ReadFull(deps.random, tokenSecret); err != nil {
		return "", fmt.Errorf("generate bootstrap token secret: %w", err)
	}
	id := hex.EncodeToString(tokenID)
	secret := hex.EncodeToString(tokenSecret)
	expiration := deps.now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-%s
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "AKS Flex Node bootstrap token"
  token-id: "%s"
  token-secret: "%s"
  expiration: "%s"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
  auth-extra-groups: "system:bootstrappers:aks-flex-node"
`, id, id, secret, expiration)
	fmt.Fprintln(deps.stderr, "INFO: creating bootstrap token")
	if _, err := deps.runner.Run(ctx, "kubectl", []string{"apply", "-f", "-"}, manifest); err != nil {
		return "", fmt.Errorf("create bootstrap token: %w", err)
	}
	return id + "." + secret, nil
}

func clusterFQDN(serverURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(serverURL))
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimSpace(serverURL)
}

func writeConfig(deps dependencies, config map[string]any, output string) error {
	rendered, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	rendered = append(rendered, '\n')
	if output == "-" {
		if _, err := deps.stdout.Write(rendered); err != nil {
			return fmt.Errorf("write config: %w", err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := os.WriteFile(output, rendered, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Chmod(output, 0o600); err != nil {
		return fmt.Errorf("set config permissions: %w", err)
	}
	fmt.Fprintf(deps.stderr, "INFO: wrote %s\n", output)
	return nil
}

const rbacManifest = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-bootstrapper
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node-bootstrapper
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-auto-approve-csr
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: aks-flex-node-role
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:node
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: system:bootstrappers:aks-flex-node
`
