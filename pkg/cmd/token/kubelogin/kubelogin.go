package kubelogin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Azure/kubelogin/pkg/token"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientauthenticationv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientauthenticationv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"
	"k8s.io/client-go/tools/auth/exec"
)

const aksAADServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"

var flagServerID string

var Command = &cobra.Command{
	Use:          "kubelogin",
	Short:        "Retrieves token via Azure/kubelogin.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context(), cmd.OutOrStdout())
	},
}

func init() {
	Command.Flags().StringVar(
		&flagServerID, "server-id", aksAADServerID,
		"The server ID to use when requesting the token.",
	)
}

func run(ctx context.Context, out io.Writer) error {
	if flagServerID == "" {
		return fmt.Errorf("server-id is required")
	}

	ec, err := resolveExecCredentialFromEnv()
	if err != nil {
		return err
	}

	tokOpts := token.OptionsWithEnv()
	tokOpts.ServerID = flagServerID
	// TODO: logging to show login details
	provider, err := token.GetTokenProvider(tokOpts)
	if err != nil {
		return err
	}
	accessToken, err := provider.GetAccessToken(ctx)
	if err != nil {
		return err
	}

	return outputToken(out, ec, accessToken)
}

const execInfoEnv = "KUBERNETES_EXEC_INFO"

func resolveExecCredentialFromEnv() (runtime.Object, error) {
	if os.Getenv(execInfoEnv) == "" {
		// we allow the env var to be empty for local testing purposes
		return &clientauthenticationv1.ExecCredential{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "client.authentication.k8s.io/v1",
				Kind:       "ExecCredential",
			},
		}, nil
	}

	ec, _, err := exec.LoadExecCredentialFromEnv()
	return ec, err
}

func outputToken(out io.Writer, ec runtime.Object, accessToken token.AccessToken) error {
	expirationTime := metav1.NewTime(accessToken.ExpiresOn)

	switch t := ec.(type) {
	case *clientauthenticationv1.ExecCredential:
		t.Status = &clientauthenticationv1.ExecCredentialStatus{
			ExpirationTimestamp: &expirationTime,
			Token:               accessToken.Token,
		}
	case *clientauthenticationv1beta1.ExecCredential:
		t.Status = &clientauthenticationv1beta1.ExecCredentialStatus{
			ExpirationTimestamp: &expirationTime,
			Token:               accessToken.Token,
		}
	default:
		return fmt.Errorf("unsupported exec credential type: %T", ec)
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")

	return enc.Encode(ec)
}
