package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	hostdaemon "github.com/Azure/AKSFlexNode/pkg/daemon"
	"github.com/Azure/AKSFlexNode/pkg/logger"
)

func newHostAgentUpgradeCommand() *cobra.Command {
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
			candidate = filepath.Clean(candidate)
			if preflight {
				return runHostAgentUpgradePreflight(cmd.Context(), cmd.OutOrStdout(), log, candidate)
			}
			result, err := hostdaemon.ActivateHostAgent(cmd.Context(), log, candidate)
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

func runHostAgentUpgradePreflight(ctx context.Context, output io.Writer, log *slog.Logger, candidate string) error {
	plan, err := hostdaemon.PreflightHostAgentActivation(ctx, log, candidate)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(output, "Candidate: %s\nActive binary: %s\nInstall target: %s\n", plan.CandidatePath, plan.ActivePath, plan.TargetPath); err != nil {
		return err
	}
	for _, action := range plan.Actions {
		if _, err := fmt.Fprintf(output, "- %s\n", action); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(output, "Preflight: no changes applied")
	return err
}
