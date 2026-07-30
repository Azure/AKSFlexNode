package bootstrap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	bootstrapflow "github.com/Azure/AKSFlexNode/pkg/bootstrap"
)

func NewCommand() *cobra.Command {
	options := bootstrapflow.DefaultOptions()
	cmd := &cobra.Command{
		Use:   "boot",
		Short: "Fetch join data, prepare the agent, and start a Flex Node",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := options.ApplyEnvironment(cmd.Flags()); err != nil {
				return err
			}
			return execute(cmd.Context(), options)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&options.AuthMode, "auth", "", "Authentication mode: msi or service-principal")
	flags.StringVar(&options.MSIClientID, "msi-client-id", "", "Optional user-assigned managed identity client ID")
	flags.StringVar(&options.SPTenantID, "sp-tenant-id", "", "Service-principal tenant ID")
	flags.StringVar(&options.SPClientID, "sp-client-id", "", "Service-principal client ID")
	flags.StringVar(&options.SPClientSecretFile, "sp-client-secret-file", "", "Protected service-principal secret file")
	flags.StringVar(&options.SPClientCertificateFile, "sp-client-certificate-file", "", "Protected PEM or unencrypted PFX certificate file")
	flags.BoolVar(&options.FetchBootstrapData, "fetch-bootstrap-data", false, "Fetch and merge fresh AKS RP bootstrap data")
	flags.StringVar(&options.BootstrapDataAPIVersion, "bootstrap-data-api-version", options.BootstrapDataAPIVersion, "AKS listBootstrapData API version")
	flags.StringVar(&options.ClusterResourceID, "cluster-resource-id", "", "Target AKS managed-cluster resource ID")
	flags.StringVar(&options.AgentPoolName, "agent-pool-name", "", "Target FlexNodes agent pool name")
	flags.StringVar(&options.ResourceManagerEndpoint, "resource-manager-endpoint", "", "Azure Resource Manager endpoint (defaults to public Azure)")
	flags.StringVar(&options.BootstrapOCIImage, "bootstrap-oci-image", "", "Override bootstrap.ociImage")
	flags.StringVar(&options.BootstrapOfflineArtifactsSource, "bootstrap-offline-artifacts-source", "", "Override bootstrap.offlineArtifacts.source")
	flags.StringArrayVar(&options.ConfigOverrides, "config-overrides", nil, "Deep-merge a JSON object; repeatable")
	flags.StringVar(&options.BaseConfigPath, "config", "", "Optional existing/base config path")
	flags.StringVar(&options.ConfigPath, "config-path", options.ConfigPath, "Rendered config destination")
	flags.StringVar(&options.AgentURL, "agent-url", "", "Agent tar.gz URL, file URL, or local path")
	flags.StringVar(&options.AgentVersion, "agent-version", "", "Agent release version used when --agent-url is omitted")
	flags.StringVar(&options.AgentSHA256, "agent-sha256", "", "Expected SHA-256 of the agent archive")
	flags.StringVar(&options.InstallDir, "install-dir", options.InstallDir, "Agent binary installation directory")
	return cmd
}

func execute(ctx context.Context, options bootstrapflow.Options) error {
	if options.BaseConfigPath != "" && !options.HasOnboardingInputs() && options.AgentURL == "" && options.AgentVersion == "" {
		return runSelf(ctx, "start", "--config", options.BaseConfigPath)
	}
	update, err := bootstrapflow.InstallAgent(ctx, options)
	if err != nil {
		return err
	}
	if update.Reexecute {
		return reexecute(update.Path, options)
	}
	data, err := bootstrapflow.BuildConfig(ctx, options)
	if err != nil {
		return err
	}
	if err := bootstrapflow.WriteConfig(options.ConfigPath, data); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "Rendered node config at %s\n", options.ConfigPath)
	if err := runSelf(ctx, "preflight", "--config", options.ConfigPath, "--output", "text"); err != nil {
		return fmt.Errorf("bootstrap preflight: %w", err)
	}
	if err := runSelf(ctx, "start", "--config", options.ConfigPath); err != nil {
		return fmt.Errorf("start node: %w", err)
	}
	return nil
}

func reexecute(path string, options bootstrapflow.Options) error {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve updated agent path: %w", err)
	}
	environment := reexecuteEnvironment(os.Environ(), options)
	arguments := append([]string{absolutePath}, os.Args[1:]...)
	if err := unix.Exec(absolutePath, arguments, environment); err != nil {
		return fmt.Errorf("execute updated agent: %w", err)
	}
	return nil
}

func reexecuteEnvironment(environment []string, options bootstrapflow.Options) []string {
	result := withoutEnvironment(environment, updateGuardEnvironment()+"=", "AKS_FLEX_NODE_SP_CLIENT_SECRET=")
	result = append(result, updateGuardEnvironment()+"=1")
	if options.SPClientSecret != "" {
		result = append(result, "AKS_FLEX_NODE_SP_CLIENT_SECRET="+options.SPClientSecret)
	}
	return result
}

func runSelf(ctx context.Context, arguments ...string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	command := exec.CommandContext(ctx, executable, arguments...) // #nosec G204 -- current executable and controlled arguments
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	command.Env = filteredEnvironment()
	if err := command.Run(); err != nil {
		return fmt.Errorf("run %s: %w", arguments[0], err)
	}
	return nil
}

func withoutEnvironment(environment []string, prefixes ...string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		keep := true
		for _, prefix := range prefixes {
			if strings.HasPrefix(value, prefix) {
				keep = false
				break
			}
		}
		if keep {
			result = append(result, value)
		}
	}
	return result
}

func filteredEnvironment() []string {
	blocked := []string{
		"AKS_FLEX_NODE_SP_CLIENT_SECRET=",
		"AKS_FLEX_NODE_AGENT_URL=",
		"AKS_FLEX_NODE_CONFIG_OVERRIDES=",
		"AKS_FLEX_NODE_BOOTSTRAP_OCI_IMAGE=",
		"AKS_FLEX_NODE_BOOTSTRAP_OFFLINE_ARTIFACTS_SOURCE=",
		updateGuardEnvironment() + "=",
	}
	return withoutEnvironment(os.Environ(), blocked...)
}

func updateGuardEnvironment() string { return "AKS_FLEX_NODE_AGENT_UPDATE_APPLIED" }
