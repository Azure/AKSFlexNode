package localmachine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
	"github.com/Azure/AKSFlexNode/pkg/aksmachine/local"
)

var (
	flagPath                    string
	flagKubernetesVersion       string
	flagSettingsVersion         string
	flagProvisioningState       string
	flagObservedSettingsVersion string
	flagMessage                 string
)

var Command = &cobra.Command{
	Use:   "local-machine",
	Short: "Mutate a local file-backed AKS machine for e2e tests.",
}

func init() {
	Command.PersistentFlags().StringVar(&flagPath, "path", "", "Path to local machine JSON file")
	_ = Command.MarkPersistentFlagRequired("path")

	createCmd := &cobra.Command{
		Use:          "create",
		Short:        "Create or replace the local machine goal state.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCreate(cmd.Context(), cmd.OutOrStdout())
		},
	}
	createCmd.Flags().StringVar(&flagKubernetesVersion, "kubernetes-version", "", "Desired Kubernetes version")
	createCmd.Flags().StringVar(&flagSettingsVersion, "settings-version", "", "Desired settings version")
	_ = createCmd.MarkFlagRequired("kubernetes-version")
	_ = createCmd.MarkFlagRequired("settings-version")

	getCmd := &cobra.Command{
		Use:          "get",
		Short:        "Print the local machine JSON.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGet(cmd.Context(), cmd.OutOrStdout())
		},
	}

	statusCmd := &cobra.Command{
		Use:          "status",
		Short:        "Patch the local machine status.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd.Context(), cmd.OutOrStdout())
		},
	}
	statusCmd.Flags().StringVar(&flagProvisioningState, "provisioning-state", "", "Provisioning state")
	statusCmd.Flags().StringVar(&flagObservedSettingsVersion, "observed-settings-version", "", "Observed settings version")
	statusCmd.Flags().StringVar(&flagMessage, "message", "", "Status message")
	_ = statusCmd.MarkFlagRequired("provisioning-state")

	deleteCmd := &cobra.Command{
		Use:          "delete",
		Short:        "Delete the local machine file.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd.Context(), cmd.OutOrStdout())
		},
	}

	Command.AddCommand(createCmd, getCmd, statusCmd, deleteCmd)
}

func client() (*local.Client, error) {
	return local.NewClient(flagPath)
}

func runCreate(ctx context.Context, out io.Writer) error {
	c, err := client()
	if err != nil {
		return err
	}
	machine, err := c.Create(ctx, aksmachine.GoalState{KubernetesVersion: flagKubernetesVersion, SettingsVersion: flagSettingsVersion})
	if err != nil {
		return err
	}
	return writeMachine(out, machine)
}

func runGet(ctx context.Context, out io.Writer) error {
	c, err := client()
	if err != nil {
		return err
	}
	machine, err := c.Get(ctx)
	if err != nil {
		return err
	}
	return writeMachine(out, machine)
}

func runStatus(ctx context.Context, out io.Writer) error {
	c, err := client()
	if err != nil {
		return err
	}
	status := aksmachine.Status{
		ProvisioningState:       aksmachine.ProvisioningState(flagProvisioningState),
		ObservedSettingsVersion: flagObservedSettingsVersion,
		Message:                 flagMessage,
	}
	if err := c.PatchStatus(ctx, status); err != nil {
		return err
	}
	machine, err := c.Get(ctx)
	if err != nil {
		return err
	}
	return writeMachine(out, machine)
}

func runDelete(_ context.Context, out io.Writer) error {
	if err := os.Remove(flagPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete local machine file %s: %w", flagPath, err)
	}
	_, err := fmt.Fprintln(out, "deleted")
	return err
}

func writeMachine(out io.Writer, machine *aksmachine.Machine) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(machine)
}
