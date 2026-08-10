package daemon

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	hostdaemon "github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
)

// NewHostAgentUpgradeCommand returns the hidden host-driven activation command.
func NewHostAgentUpgradeCommand() *cobra.Command {
	var preflight bool
	cmd := &cobra.Command{
		Use:    "agent-upgrade",
		Short:  "Activate this executable as the host agent daemon",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			candidate, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolve candidate executable: %w", err)
			}
			candidate, err = filepath.Abs(candidate)
			if err != nil {
				return fmt.Errorf("resolve absolute candidate executable path: %w", err)
			}
			log := logger.CreateLogger("info", "")
			if preflight {
				plan, err := hostdaemon.PreflightHostAgentActivation(cmd.Context(), log, filepath.Clean(candidate))
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Candidate: %s\nActive binary: %s\nInstall target: %s\n", plan.CandidatePath, plan.ActivePath, plan.TargetPath); err != nil {
					return err
				}
				for _, action := range plan.Actions {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", action); err != nil {
						return err
					}
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Preflight: no changes applied")
				return err
			}
			result, err := hostdaemon.ActivateHostAgent(cmd.Context(), log, filepath.Clean(candidate))
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "activated host agent daemon: %s -> %s\n", result.PreviousPath, result.CurrentPath)
			return err
		},
	}
	cmd.Flags().BoolVar(&preflight, "preflight", false, "Show and validate the host activation plan without applying it")
	return cmd
}
