package localmachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/aksmachine"
)

const fileMode = 0o600

const localResourceID = "local-test-machine"

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

func runCreate(ctx context.Context, out io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	machine := &aksmachine.Machine{
		ID: localResourceID,
		Goal: aksmachine.GoalState{
			KubernetesVersion: flagKubernetesVersion,
			SettingsVersion:   flagSettingsVersion,
		},
	}
	if err := writeLocalMachine(flagPath, machine); err != nil {
		return err
	}
	return writeMachine(out, machine)
}

func runGet(ctx context.Context, out io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	machine, err := readLocalMachine(flagPath)
	if err != nil {
		return err
	}
	return writeMachine(out, machine)
}

func runStatus(ctx context.Context, out io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	machine, err := readLocalMachine(flagPath)
	if err != nil {
		return err
	}
	status := aksmachine.Status{
		ProvisioningState:       aksmachine.ProvisioningState(flagProvisioningState),
		ObservedSettingsVersion: flagObservedSettingsVersion,
		Message:                 flagMessage,
	}
	machine.Status = status
	if err := writeLocalMachine(flagPath, machine); err != nil {
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

func readLocalMachine(path string) (*aksmachine.Machine, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, &aksmachine.NotFoundError{Resource: path}
	}
	if err != nil {
		return nil, fmt.Errorf("read machine file %s: %w", path, err)
	}

	var machine aksmachine.Machine
	if err := json.Unmarshal(data, &machine); err != nil {
		return nil, fmt.Errorf("decode machine file %s: %w", path, err)
	}
	return &machine, nil
}

func writeLocalMachine(path string, machine *aksmachine.Machine) error {
	if machine == nil {
		return fmt.Errorf("machine is nil")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create local machine file directory: %w", err)
	}
	data, err := json.MarshalIndent(machine, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal machine: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Clean(path), data, fileMode); err != nil {
		return fmt.Errorf("write machine file %s: %w", path, err)
	}
	return nil
}
