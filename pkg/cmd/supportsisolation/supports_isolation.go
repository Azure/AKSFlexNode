package supportsisolation

import (
	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// NewCommand exposes the capability query used by SDK daemon upgrades and recovery.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "supports-isolation MODE",
		Hidden:       true,
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return config.ValidateIsolation(isolation.Mode(args[0]))
		},
	}
}
