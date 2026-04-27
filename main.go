package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/cmd/token"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "aks-flex-node",
		Short: "AKS Flex Node Agent",
		Long:  "Azure Kubernetes Service Flex Node Agent for edge computing scenarios",
	}

	rootCmd.AddCommand(NewAgentCommand())
	rootCmd.AddCommand(NewUnbootstrapCommand())
	rootCmd.AddCommand(NewVersionCommand())
	rootCmd.AddCommand(token.Command)

	// Set up context with signal handling
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Execute command with context
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Command execution failed: %v\n", err)
		os.Exit(1)
	}
}
