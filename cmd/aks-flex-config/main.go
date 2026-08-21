package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	configcmd "github.com/Azure/AKSFlexNode/pkg/cmd/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := configcmd.NewCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}
