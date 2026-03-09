package token

import (
	"github.com/spf13/cobra"

	"github.com/Azure/AKSFlexNode/pkg/cmd/token/arc"
	"github.com/Azure/AKSFlexNode/pkg/cmd/token/kubelogin"
)

var Command = &cobra.Command{
	Use:   "token",
	Short: "Kubernetes exec based authentication provider.",
}

func init() {
	Command.AddCommand(arc.Command)
	Command.AddCommand(kubelogin.Command)
}
