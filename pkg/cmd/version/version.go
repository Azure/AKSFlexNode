package version

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version information variables (set at build time)
var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show version information",
		Long:  "Display version, build commit, and build time information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("AKS Flex Node Agent\n")
			fmt.Printf("Version: %s\n", Version)
			fmt.Printf("Git Commit: %s\n", GitCommit)
			fmt.Printf("Build Time: %s\n", BuildTime)
		},
	}
}
