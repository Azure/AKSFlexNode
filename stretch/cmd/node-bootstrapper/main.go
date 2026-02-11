package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.goms.io/aks/AKSFlexNode/pkg/units"
)

var (
	flagOSRootDir    string
	flagStoreRootDir string
	flagConfigPath   string
)

var Command = cobra.Command{
	Use: "node-bootstrapper",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagOSRootDir == "" {
			// use a tmp dir for now
			tmpDir, err := os.MkdirTemp("", "node-bootstrapper-*")
			if err != nil {
				return fmt.Errorf("creating temp dir for state: %w", err)
			}
			flagOSRootDir = tmpDir
		}
		if flagStoreRootDir == "" {
			flagStoreRootDir = filepath.Join(flagOSRootDir, "aks-flex-node")
		}

		fmt.Printf("Using OS root dir: %s\n", flagOSRootDir)
		fmt.Printf("Using store root dir: %s\n", flagStoreRootDir)

		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context())
	},
	// SilenceErrors: true,
	SilenceUsage: true,
}

func init() {
	Command.Flags().StringVar(&flagOSRootDir, "os-root-dir", "", "")
	Command.Flags().StringVar(&flagStoreRootDir, "store-root-dir", "", "")
	Command.Flags().StringVar(&flagConfigPath, "config-path", "", "Path to the node-bootstrapper configuration file.")

	Command.MarkFlagRequired("config-path")
}

func main() {
	if err := Command.Execute(); err != nil {
		os.Exit(1)
	}
}

func readConfig(path string) (units.OverlayConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return units.OverlayConfig{}, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	var rv units.OverlayConfig
	if err := dec.Decode(&rv); err != nil {
		return units.OverlayConfig{}, fmt.Errorf("decoding config JSON: %w", err)
	}
	return rv, nil
}

func run(ctx context.Context) error {
	config, err := readConfig(flagConfigPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	overlay := units.NewOverlay(
		config,
		flagStoreRootDir,
		flagOSRootDir,
		nil,
	)

	return overlay.Apply(ctx)
}
