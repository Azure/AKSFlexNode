package daemon

import (
	"fmt"

	"github.com/spf13/cobra"

	hostdaemon "github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
)

// newAgentUpgradeRecoveryCommand remains callable only as a local root
// operation by the systemd recovery unit.
func newAgentUpgradeRecoveryCommand() *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use:    "recover-agent-upgrade",
		Short:  "Restore the last-known-good agent after a failed upgrade",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := logger.CreateLogger("info", "")
			if err := hostdaemon.RecoverAgentUpgrade(cmd.Context(), log, message); err != nil {
				return fmt.Errorf("recover AgentUpgrade: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "failure message to publish")
	return cmd
}
