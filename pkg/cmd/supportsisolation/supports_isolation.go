package supportsisolation

import (
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/spf13/cobra"
)

// NewCommand implements the existing Unbounded candidate compatibility probe.
func NewCommand() *cobra.Command {
	return &cobra.Command{Use: "supports-isolation MODE", Hidden: true, Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return config.ValidateIsolation(isolation.Mode(args[0]))
		},
	}
}
