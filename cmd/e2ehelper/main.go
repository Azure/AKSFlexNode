package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/cmd/e2ehelper/localmachine"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "e2ehelper",
		Short: "AKS Flex Node E2E helper",
	}
	rootCmd.AddCommand(localmachine.Command)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "Command execution failed: %v\n", err)
		os.Exit(1)
	}
}
