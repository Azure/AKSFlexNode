package controller

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/AKSFlexNode/pkg/flexcontroller"
)

func NewCommand() *cobra.Command {
	var opts flexcontroller.Options
	opts.EnableCSRApprover = true
	cmd := &cobra.Command{
		Use:   "aks-flex-controller",
		Short: "Run the AKS Flex in-cluster machine endpoint",
		Long: "Run the read-only in-cluster AKS Machine endpoint used by AKS Flex Node daemons " +
			"through the Kubernetes API server service proxy.",
		RunE: func(cmd *cobra.Command, args []string) error {
			log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
			if err := flexcontroller.Run(cmd.Context(), opts, log); err != nil {
				return fmt.Errorf("run aks-flex-controller: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.ListenAddress, "listen-address", flexcontroller.DefaultListenAddress, "Address for the HTTP server to listen on")
	cmd.Flags().StringVar(&opts.ClusterResourceID, "cluster-resource-id", "", "ARM resource ID of the target AKS managed cluster (required)")
	cmd.Flags().StringVar(&opts.AgentPoolName, "agent-pool-name", "aksflexnodes", "AKS agent pool containing FlexNode machines")
	cmd.Flags().StringVar(&opts.ResourceManagerEndpoint, "resource-manager-endpoint", config.DefaultResourceManagerEndpointURL, "Azure Resource Manager endpoint")
	cmd.Flags().DurationVar(&opts.ShutdownTimeout, "shutdown-timeout", 15*time.Second, "Graceful shutdown timeout")
	cmd.Flags().BoolVar(&opts.EnableCSRApprover, "enable-csr-approver", true, "Approve daemon-controller CSRs for pre-created AKS Machines")
	cmd.Flags().StringVar(&opts.Kubeconfig, "kubeconfig", "", "Path to kubeconfig for CSR approver; empty uses in-cluster/default loading rules")
	cmd.Flags().StringVar(&opts.BootstrapGroup, "bootstrap-group", flexcontroller.DefaultBootstrapGroup, "Bootstrap requester group allowed to request daemon certificates")
	cmd.Flags().StringVar(&opts.DaemonGroup, "daemon-group", flexcontroller.DefaultDaemonGroup, "Daemon certificate group to approve")
	cmd.Flags().Int32Var(&opts.MaxExpirationSeconds, "max-expiration-seconds", 0, "Maximum daemon certificate expiration in seconds; 0 uses the approver default")
	_ = cmd.MarkFlagRequired("cluster-resource-id")
	return cmd
}
