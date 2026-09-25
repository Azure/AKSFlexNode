// Package hostroot provides the hidden host-root command.
package hostroot

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
)

// NewCommand returns the host-root command. It prints where this release keeps
// its host-side files on this host, as it will once the host root is migrated,
// and changes nothing.
//
// Its existence is what the install scripts and AgentUpgrade check for: a
// release without it predates the host root and installs under /usr/local.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "host-root",
		Short:  "Print the directory that holds the agent's host-side files",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), daemon.PlannedHostRoot())
			return err
		},
	}
}
