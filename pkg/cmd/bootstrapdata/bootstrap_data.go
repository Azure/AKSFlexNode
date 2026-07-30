package bootstrapdata

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/bootstrapdata"
)

func NewCommand() *cobra.Command {
	options := bootstrapdata.Options{
		ResourceManagerEndpoint: bootstrapdata.DefaultResourceManagerEndpoint,
		AuthorityHost:           bootstrapdata.DefaultAuthorityHost,
		APIVersion:              bootstrapdata.DefaultAPIVersion,
	}
	cmd := &cobra.Command{
		Use:   "fetch-bootstrap-data",
		Short: "Fetch current FlexNodes join data from AKS RP",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			options.SPClientSecret = os.Getenv("AKS_FLEX_NODE_SP_CLIENT_SECRET")
			_ = os.Unsetenv("AKS_FLEX_NODE_SP_CLIENT_SECRET")
			return bootstrapdata.FetchAndWrite(cmd.Context(), options)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&options.ClusterResourceID, "cluster-resource-id", "", "Target AKS managed-cluster resource ID")
	flags.StringVar(&options.AgentPoolName, "agent-pool-name", "", "Target FlexNodes agent pool name")
	flags.StringVar(&options.AuthMode, "auth", "", "Authentication mode: msi or service-principal")
	flags.StringVar(&options.MSIClientID, "msi-client-id", "", "Optional user-assigned managed identity client ID")
	flags.StringVar(&options.SPTenantID, "sp-tenant-id", "", "Service-principal tenant ID")
	flags.StringVar(&options.SPClientID, "sp-client-id", "", "Service-principal client ID")
	flags.StringVar(&options.SPClientSecretFile, "sp-client-secret-file", "", "Protected service-principal client-secret file")
	flags.StringVar(&options.SPClientCertificateFile, "sp-client-certificate-file", "", "Protected PEM or unencrypted PFX certificate file")
	flags.StringVar(&options.SPClientCredentialFile, "sp-client-credential-file", "", "Protected secret or certificate file, detected by content")
	flags.StringVar(&options.ResourceManagerEndpoint, "resource-manager-endpoint", options.ResourceManagerEndpoint, "Azure Resource Manager endpoint")
	flags.StringVar(&options.AuthorityHost, "authority-host", options.AuthorityHost, "Microsoft Entra authority host")
	flags.StringVar(&options.APIVersion, "api-version", options.APIVersion, "AKS listBootstrapData API version")
	flags.StringVarP(&options.OutputPath, "output", "o", "", "Protected JSON output path (required)")
	_ = cmd.MarkFlagRequired("cluster-resource-id")
	_ = cmd.MarkFlagRequired("agent-pool-name")
	_ = cmd.MarkFlagRequired("auth")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}
