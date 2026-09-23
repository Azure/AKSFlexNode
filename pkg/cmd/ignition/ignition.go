// Package ignition implements `aks-flex-node ignition`, which renders an Ignition config that
// bootstraps a host on first boot. Hosts such as Azure Container Linux are provisioned only by
// Ignition and have a read-only /usr, so the agent goes under a host prefix.
package ignition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	agentconfig "github.com/Azure/unbounded/pkg/agent/config"

	"github.com/Azure/AKSFlexNode/pkg/config"
)

// defaultHostPrefix is used when neither --host-prefix nor the base config sets one. Hosts
// provisioned by Ignition typically have a read-only /usr, so the bootstrap.sh default of
// /usr/local does not work there.
const defaultHostPrefix = "/opt/aks-flex-node"

// bootstrapValueFlags and bootstrapSwitches are the bootstrap.sh options that take a value and
// those that do not. TestBootstrapFlagsMatchTheScript keeps them in step with the embedded script.
var (
	bootstrapValueFlags = []string{
		"--auth",
		"--msi-client-id",
		"--sp-tenant-id",
		"--sp-client-id",
		"--sp-client-secret-file",
		"--sp-client-certificate-file",
		"--agent-url",
		"--agent-version",
		"--agent-sha256",
		"--bootstrap-data-api-version",
		"--cluster-resource-id",
		"--agent-pool-name",
		"--resource-manager-endpoint",
		"--bootstrap-oci-image",
		"--bootstrap-offline-artifacts-source",
		"--config-overrides",
		"--host-prefix",
		"--install-dir",
		"--config-path",
	}
	bootstrapSwitches = []string{"--fetch-bootstrap-data"}
)

// ownedBootstrapFlags are bootstrap.sh options this command sets itself, with the reason a caller
// cannot pass them.
var ownedBootstrapFlags = map[string]string{
	"--host-prefix":                "use this command's --host-prefix",
	"--install-dir":                "the agent is installed under the host prefix",
	"--config-path":                "the agent unit reads the default config path",
	"--sp-client-secret-file":      "use this command's --sp-client-secret-file, which also writes the file to the host",
	"--sp-client-certificate-file": "use this command's --sp-client-certificate-file, which also writes the file to the host",
}

type options struct {
	baseConfigPath          string
	hostPrefix              string
	spClientSecretFile      string
	spClientCertificateFile string
	outputPath              string
}

// NewCommand returns the ignition command.
func NewCommand() *cobra.Command {
	var opts options

	cmd := &cobra.Command{
		Use:   "ignition [flags] -- BOOTSTRAP_ARGS...",
		Short: "Render an Ignition config that bootstraps the host on first boot",
		Long: `Render an Ignition config for hosts provisioned by Ignition, such as Azure Container Linux.

The config writes bootstrap.sh and any service principal credential to the host, and enables a
unit that runs bootstrap.sh with BOOTSTRAP_ARGS once the network is up. The unit retries until the
agent is installed and does not run after that. The script, which carries the base config, is
removed once bootstrap succeeds.

The agent is installed under --host-prefix, because /usr is read-only on these hosts. The output
contains the base config and credentials, so treat it as a secret.`,
		Example: `  aks-flex-node ignition --base-config base.json -o node.ign -- \
    --auth msi --agent-version v0.1.0 --fetch-bootstrap-data`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return run(cmd, opts, args)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.baseConfigPath, "base-config", "", "Base config JSON for bootstrap.sh; without it bootstrap.sh needs --fetch-bootstrap-data")
	flags.StringVar(&opts.hostPrefix, "host-prefix", "", "Host install prefix for the agent (default "+defaultHostPrefix+", or agent.hostPrefix from the base config)")
	flags.StringVar(&opts.spClientSecretFile, "sp-client-secret-file", "", "Service principal client secret file to write to the host")
	flags.StringVar(&opts.spClientCertificateFile, "sp-client-certificate-file", "", "Service principal client certificate file to write to the host")
	flags.StringVarP(&opts.outputPath, "output", "o", "-", "Output path, or - for stdout; a file is created with mode 0600")
	cmd.MarkFlagsMutuallyExclusive("sp-client-secret-file", "sp-client-certificate-file")

	return cmd
}

func run(cmd *cobra.Command, opts options, bootstrapArgs []string) error {
	if dash := cmd.ArgsLenAtDash(); len(bootstrapArgs) > 0 && dash != 0 {
		return errors.New("put bootstrap.sh options after --")
	}

	in, err := buildRenderInput(opts, cmd.Flags().Changed("host-prefix"), bootstrapArgs)
	if err != nil {
		return err
	}
	out, err := render(in)
	if err != nil {
		return err
	}

	return writeOutput(cmd.OutOrStdout(), opts.outputPath, out)
}

// buildRenderInput validates everything before anything is rendered, so a mistake is reported
// here rather than by a host that fails on first boot.
func buildRenderInput(opts options, hostPrefixSet bool, bootstrapArgs []string) (renderInput, error) {
	seen, err := validateBootstrapArgs(bootstrapArgs)
	if err != nil {
		return renderInput{}, err
	}
	if !seen["--agent-url"] && !seen["--agent-version"] {
		return renderInput{}, errors.New("bootstrap.sh needs --agent-url or --agent-version after --")
	}

	var in renderInput
	in.bootstrapArgs = bootstrapArgs

	configPrefix := ""
	if opts.baseConfigPath != "" {
		in.baseConfig, configPrefix, err = loadBaseConfig(opts.baseConfigPath)
		if err != nil {
			return renderInput{}, err
		}
	} else if !seen["--fetch-bootstrap-data"] {
		return renderInput{}, errors.New("without --base-config, bootstrap.sh needs --fetch-bootstrap-data after --")
	}

	in.hostPrefix, err = resolveHostPrefix(opts.hostPrefix, hostPrefixSet, configPrefix)
	if err != nil {
		return renderInput{}, err
	}

	in.credential, err = loadCredential(opts)
	if err != nil {
		return renderInput{}, err
	}

	return in, nil
}

// validateBootstrapArgs checks the arguments against bootstrap.sh's parser and returns the options
// that were given.
func validateBootstrapArgs(args []string) (map[string]bool, error) {
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if reason, ok := ownedBootstrapFlags[arg]; ok {
			return nil, fmt.Errorf("bootstrap.sh option %s cannot be passed through: %s", arg, reason)
		}

		switch {
		case slices.Contains(bootstrapSwitches, arg):
		case slices.Contains(bootstrapValueFlags, arg):
			if i+1 >= len(args) || args[i+1] == "" {
				return nil, fmt.Errorf("bootstrap.sh option %s requires a value", arg)
			}
			i++
			if err := validateUnitArgument(arg, args[i]); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown bootstrap.sh option %q", arg)
		}
		seen[arg] = true
	}

	return seen, nil
}

// validateUnitArgument rejects values that cannot be written into the unit's command line.
func validateUnitArgument(flag, value string) error {
	if strings.ContainsFunc(value, unicode.IsControl) {
		return fmt.Errorf("value of bootstrap.sh option %s contains a control character", flag)
	}

	return nil
}

// loadBaseConfig returns the base config as compact JSON, and the host prefix it sets.
func loadBaseConfig(configPath string) ([]byte, string, error) {
	raw, err := os.ReadFile(configPath) // #nosec G304 -- the operator names the file to embed
	if err != nil {
		return nil, "", fmt.Errorf("read base config: %w", err)
	}

	var base struct {
		Agent struct {
			HostPrefix string `json:"hostPrefix"`
		} `json:"agent"`
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, "", fmt.Errorf("base config %s must be a JSON object", configPath)
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		return nil, "", fmt.Errorf("base config %s: %w", configPath, err)
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, "", fmt.Errorf("base config %s: %w", configPath, err)
	}

	return compact.Bytes(), base.Agent.HostPrefix, nil
}

func resolveHostPrefix(flagPrefix string, flagSet bool, configPrefix string) (string, error) {
	prefix := defaultHostPrefix
	switch {
	case flagSet && configPrefix != "" && path.Clean(flagPrefix) != path.Clean(configPrefix):
		return "", fmt.Errorf("--host-prefix %s does not match agent.hostPrefix %s in the base config", flagPrefix, configPrefix)
	case flagSet:
		prefix = flagPrefix
	case configPrefix != "":
		prefix = configPrefix
	}

	if prefix == "" {
		return "", errors.New("--host-prefix must not be empty")
	}
	// ValidateHostPrefix trims before checking, but the prefix is used as given.
	if strings.TrimSpace(prefix) != prefix {
		return "", fmt.Errorf("host prefix %q must not have surrounding whitespace", prefix)
	}
	if err := agentconfig.ValidateHostPrefix(prefix); err != nil {
		return "", err
	}

	return prefix, nil
}

// loadCredential reads the service principal credential to write to the host. The checks are the
// ones bootstrap.sh and the agent apply on the host, made here so that they fail early.
func loadCredential(opts options) (*credential, error) {
	switch {
	case opts.spClientSecretFile != "":
		content, err := config.LoadServicePrincipalCredentialFile(opts.spClientSecretFile)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(content)) == 0 {
			return nil, errors.New("service principal client secret file is empty")
		}

		return &credential{
			flag:     "--sp-client-secret-file",
			hostPath: path.Join(credentialsDir, "sp-client-secret"),
			content:  content,
		}, nil
	case opts.spClientCertificateFile != "":
		if err := config.ValidateServicePrincipalCertificateFile(opts.spClientCertificateFile); err != nil {
			return nil, err
		}
		content, err := config.LoadServicePrincipalCredentialFile(opts.spClientCertificateFile)
		if err != nil {
			return nil, err
		}
		// PKCS#12 is recognized by its suffix, so the host copy keeps it.
		name := "sp-client-certificate"
		if strings.HasSuffix(strings.ToLower(opts.spClientCertificateFile), ".pfx") {
			name += ".pfx"
		}

		return &credential{
			flag:     "--sp-client-certificate-file",
			hostPath: path.Join(credentialsDir, name),
			content:  content,
		}, nil
	default:
		return nil, nil
	}
}

func writeOutput(stdout io.Writer, outputPath string, out []byte) error {
	if outputPath == "-" {
		_, err := stdout.Write(out)
		return err
	}

	// O_EXCL keeps an existing file, possibly readable by others, from being reused for secrets.
	f, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator names the output file
	if err != nil {
		return fmt.Errorf("create output: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		_ = f.Close()
		return fmt.Errorf("write output: %w", err)
	}

	return f.Close()
}
