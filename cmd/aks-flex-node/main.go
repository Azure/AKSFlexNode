package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/cmd/bootstrapdata"
	"github.com/Azure/AKSFlexNode/pkg/cmd/daemon"
	"github.com/Azure/AKSFlexNode/pkg/cmd/nspawnlifecycle"
	"github.com/Azure/AKSFlexNode/pkg/cmd/preflight"
	"github.com/Azure/AKSFlexNode/pkg/cmd/reset"
	"github.com/Azure/AKSFlexNode/pkg/cmd/start"
	"github.com/Azure/AKSFlexNode/pkg/cmd/token"
	"github.com/Azure/AKSFlexNode/pkg/cmd/version"
)

func main() {
	rootCmd := newRootCommand()

	// Set up context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Execute command with context
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "Command execution failed: %v\n", err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "aks-flex-node",
		Short: "AKS Flex Node Agent",
		Long:  "Azure Kubernetes Service Flex Node Agent for edge computing scenarios",
	}

	rootCmd.AddCommand(start.NewCommand())
	rootCmd.AddCommand(bootstrapdata.NewCommand())
	rootCmd.AddCommand(preflight.NewCommand())
	rootCmd.AddCommand(daemon.NewCommand())
	rootCmd.AddCommand(nspawnlifecycle.NewCommand())
	rootCmd.AddCommand(reset.NewCommand())
	rootCmd.AddCommand(version.NewCommand())
	rootCmd.AddCommand(token.Command)

	return rootCmd
}
