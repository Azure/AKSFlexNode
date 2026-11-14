package supportsisolation

import (
	"fmt"

	"github.com/spf13/cobra"
)

// NewCommand provides the capability probe used before activating an agent
// binary on a node whose mode older binaries cannot operate.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "supports-isolation MODE",
		Short:  "Check whether the agent supports a node isolation mode",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			switch args[0] {
			case "nspawn", "none", "container":
				return nil
			default:
				return fmt.Errorf("unsupported node isolation %q", args[0])
			}
		},
	}
}
