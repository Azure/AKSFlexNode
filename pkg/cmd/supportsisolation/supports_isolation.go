package supportsisolation

import (
	"errors"
	"fmt"

	"github.com/Azure/unbounded/pkg/agent/isolation"
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
			if args[0] == "" {
				return errors.New("no isolation mode named")
			}
			mode, err := isolation.ParseMode(args[0])
			if err != nil {
				return err
			}
			if mode == isolation.ModeContainer {
				return fmt.Errorf("isolation mode %q is not supported", mode)
			}
			return nil
		},
	}
}
