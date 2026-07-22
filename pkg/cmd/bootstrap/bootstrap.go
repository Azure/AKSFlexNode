package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/renameio/v2"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/nodebootstrap"
)

const defaultConfigPath = config.ConfigDir + "/config.json"

type options struct {
	startConfigURL string
	configPath     string

	agentBinaryURL    string
	agentBinarySHA256 string
	agentBinaryFormat string
	agentBinaryPath   string

	storageAuth             string
	storageTenantID         string
	storageClientID         string
	storageClientSecretFile string
	storageAuthorityHost    string
	storageTokenScope       string

	jqQueries  []string
	jqArgs     []string
	jqJSONArgs []string

	ignorePreflightErrors []string
	failOnWarnings        bool
}

type handler struct {
	options       options
	executable    func() (string, error)
	reexec        func(string) error
	runCommand    func(context.Context, string, ...string) error
	writeConfig   func(string, []byte) error
	newDownloader func(nodebootstrap.StorageAuthOptions) (*nodebootstrap.Downloader, error)
}

// NewCommand returns the end-to-end bootstrap command. The existing start
// command remains available for callers that already have a complete config.
func NewCommand() *cobra.Command {
	h := &handler{
		executable:    os.Executable,
		reexec:        reexec,
		runCommand:    runCommand,
		writeConfig:   writeConfig,
		newDownloader: nodebootstrap.NewDownloader,
	}
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Prepare configuration, run preflight, and start the node",
		Long: "Optionally update the baked agent, retrieve a partial start configuration, " +
			"apply live defaults and gojq transformations, run preflight, and start the node.",
		Example: `  # SAS is embedded in each signed URL.
  aks-flex-node bootstrap \
    --start-config-url "$START_CONFIG_URL" \
    --agent-binary-url "$AGENT_BINARY_URL" \
    --storage-auth sas

  # Service-principal secret is read from a protected file.
  aks-flex-node bootstrap \
    --start-config-url "$START_CONFIG_URL" \
    --storage-auth service-principal \
    --storage-tenant-id "$TENANT_ID" \
    --storage-client-id "$CLIENT_ID" \
    --storage-client-secret-file /run/credentials/storage-client-secret

  # System-assigned MSI; add --storage-client-id for a user-assigned identity.
  aks-flex-node bootstrap \
    --start-config-url "$START_CONFIG_URL" \
    --storage-auth msi

  # Optional runtime customization uses jq variables without changing join credentials.
  aks-flex-node bootstrap \
    --start-config-url "$START_CONFIG_URL" \
    --storage-auth msi \
    --jq-arg scenario=edge-vhd \
    --jq '.node.labels["aks-flex-node.azure.com/bootstrap-scenario"] = $scenario'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return h.execute(cmd.Context())
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&h.options.startConfigURL, "start-config-url", "", "Partial node start config URL, file:// URL, or local path")
	flags.StringVar(&h.options.configPath, "config", defaultConfigPath, "Final config path, or an existing config when --start-config-url is omitted")
	flags.StringVar(&h.options.agentBinaryURL, "agent-binary-url", "", "Optional agent binary/archive URL, file:// URL, or local path")
	flags.StringVar(&h.options.agentBinarySHA256, "agent-binary-sha256", "", "Expected SHA-256 of the downloaded agent artifact")
	flags.StringVar(&h.options.agentBinaryFormat, "agent-binary-format", "auto", "Agent artifact format: auto, binary, or tar.gz")
	flags.StringVar(&h.options.agentBinaryPath, "agent-binary-path", "/usr/local/bin/aks-flex-node", "Active agent binary destination")
	flags.StringVar(&h.options.storageAuth, "storage-auth", "none", "Storage auth: none, sas, service-principal, or msi")
	flags.StringVar(&h.options.storageTenantID, "storage-tenant-id", "", "Service-principal tenant ID")
	flags.StringVar(&h.options.storageClientID, "storage-client-id", "", "Service-principal or user-assigned managed identity client ID")
	flags.StringVar(&h.options.storageClientSecretFile, "storage-client-secret-file", "", "Protected service-principal client secret file")
	flags.StringVar(&h.options.storageAuthorityHost, "storage-authority-host", "", "Optional Microsoft Entra authority host")
	flags.StringVar(&h.options.storageTokenScope, "storage-token-scope", nodebootstrap.DefaultStorageScope, "Azure Storage OAuth token scope")
	flags.StringArrayVar(&h.options.jqQueries, "jq", nil, "gojq config transformation; repeatable")
	flags.StringArrayVar(&h.options.jqArgs, "jq-arg", nil, "Bind a gojq string variable as NAME=VALUE; repeatable")
	flags.StringArrayVar(&h.options.jqJSONArgs, "jq-argjson", nil, "Bind a JSON-typed gojq variable as NAME=JSON; repeatable")
	flags.StringSliceVar(&h.options.ignorePreflightErrors, "ignore-preflight-errors", nil, "Preflight errors to report as warnings")
	flags.BoolVar(&h.options.failOnWarnings, "fail-on-warnings", false, "Fail when preflight returns a warning")

	return cmd
}

func (h *handler) execute(ctx context.Context) error {
	hasStartConfig := strings.TrimSpace(h.options.startConfigURL) != ""
	hasAgentUpdate := strings.TrimSpace(h.options.agentBinaryURL) != ""
	if !hasStartConfig && len(h.options.jqQueries) > 0 {
		return fmt.Errorf("--jq requires --start-config-url")
	}
	executablePath, err := h.executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if !hasStartConfig && !hasAgentUpdate {
		if _, err := os.Stat(h.options.configPath); err != nil {
			return fmt.Errorf("existing config is required when --start-config-url is omitted: %w", err)
		}
		return h.runCommand(ctx, executablePath, "start", "--config", h.options.configPath)
	}

	downloader, err := h.newDownloader(nodebootstrap.StorageAuthOptions{
		Mode:             h.options.storageAuth,
		TenantID:         h.options.storageTenantID,
		ClientID:         h.options.storageClientID,
		ClientSecretFile: h.options.storageClientSecretFile,
		AuthorityHost:    h.options.storageAuthorityHost,
		TokenScope:       h.options.storageTokenScope,
	})
	if err != nil {
		return fmt.Errorf("configure bootstrap downloads: %w", err)
	}

	guardSet := os.Getenv(nodebootstrap.AgentUpdateGuardEnv) == "1"
	if hasAgentUpdate && !guardSet {
		result, err := nodebootstrap.UpdateAgent(ctx, downloader, nodebootstrap.AgentUpdateOptions{
			Source:         h.options.agentBinaryURL,
			SHA256:         h.options.agentBinarySHA256,
			Format:         h.options.agentBinaryFormat,
			Destination:    h.options.agentBinaryPath,
			UpdateGuardSet: guardSet,
		})
		if err != nil {
			return err
		}
		if result.Updated {
			_, _ = fmt.Fprintln(os.Stderr, "Agent update installed; continuing with the updated binary")
		}
		return h.reexec(result.Path)
	}

	if !hasStartConfig {
		if _, err := os.Stat(h.options.configPath); err != nil {
			return fmt.Errorf("existing config is required when --start-config-url is omitted: %w", err)
		}
		return h.runCommand(ctx, executablePath, "start", "--config", h.options.configPath)
	}

	finalData, _, err := nodebootstrap.FetchAndRenderConfig(ctx, downloader, h.options.startConfigURL, nodebootstrap.ConfigRenderOptions{
		Queries:    h.options.jqQueries,
		StringArgs: h.options.jqArgs,
		JSONArgs:   h.options.jqJSONArgs,
	})
	if err != nil {
		return err
	}
	if err := h.writeConfig(h.options.configPath, finalData); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Rendered node config at %s\n", h.options.configPath)

	preflightArgs := []string{"preflight", "--config", h.options.configPath, "--output", "text"}
	if len(h.options.ignorePreflightErrors) > 0 {
		preflightArgs = append(preflightArgs, "--ignore-preflight-errors", strings.Join(h.options.ignorePreflightErrors, ","))
	}
	if h.options.failOnWarnings {
		preflightArgs = append(preflightArgs, "--fail-on-warnings")
	}
	if err := h.runCommand(ctx, executablePath, preflightArgs...); err != nil {
		return fmt.Errorf("bootstrap preflight: %w", err)
	}
	if err := h.runCommand(ctx, executablePath, "start", "--config", h.options.configPath); err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	return nil
}

func writeConfig(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if err := renameio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("atomically write node config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("set node config permissions: %w", err)
	}
	return nil
}

func reexec(path string) error {
	guardPrefix := nodebootstrap.AgentUpdateGuardEnv + "="
	environment := make([]string, 0, len(os.Environ())+1)
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, guardPrefix) {
			environment = append(environment, value)
		}
	}
	environment = append(environment, guardPrefix+"1")
	arguments := append([]string{path}, os.Args[1:]...)
	if err := unix.Exec(path, arguments, environment); err != nil {
		return fmt.Errorf("execute updated agent: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, path string, arguments ...string) error {
	// #nosec G204 -- path is the current executable or the explicitly configured, verified update destination.
	command := exec.CommandContext(ctx, path, arguments...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	command.Env = storageSecretFilteredEnvironment()
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", arguments[0], err)
	}
	return nil
}

func storageSecretFilteredEnvironment() []string {
	const storageSecretEnvironmentPrefix = "AKS_FLEX_NODE_STORAGE_CLIENT_SECRET=" // #nosec G101 -- variable name, not a credential.
	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, storageSecretEnvironmentPrefix) {
			environment = append(environment, value)
		}
	}
	return environment
}
