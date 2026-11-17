package nodelifecycle

import (
	"log/slog"

	"github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/spf13/cobra"
)

// NewCommand supplies the OCI worker unit hooks without changing nspawn hooks.
func NewCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "node-lifecycle", Hidden: true}
	for _, phase := range []string{"pre-start", "post-start", "reconcile"} {
		cmd.AddCommand(&cobra.Command{Use: phase + " SLOT", Hidden: true, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return daemon.RunNodeLifecycle(cmd.Context(), slog.Default(), phase, args[0])
			},
		})
	}
	return cmd
}
