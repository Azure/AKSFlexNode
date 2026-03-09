package arc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/pkg/apis/clientauthentication/install"
	clientauthenticationv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientauthenticationv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"

	"github.com/Azure/AKSFlexNode/pkg/auth"
)

const (
	aksAADServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"
	// Kubernetes API server scope - this is what kubelet needs
	kubernetesScope = "6dae42f8-4368-4678-94ff-3960e28e3630/.default"
)

var flagServerID string

var Command = &cobra.Command{
	Use:          "arc-credential",
	Short:        "Retrieves token via Azure Arc managed identity.",
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
		return fmt.Errorf("failed to resolve exec credential: %w", err)
	}

	// Create Arc authentication provider
	authProvider := auth.NewAuthProvider()

	// Get Arc managed identity credential
	cred, err := authProvider.ArcCredential()
	if err != nil {
		return fmt.Errorf("failed to create Arc credential: %w", err)
	}

	// Get access token using Arc MSI with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	accessToken, err := authProvider.GetAccessTokenForResource(timeoutCtx, cred, kubernetesScope)
	if err != nil {
		return fmt.Errorf("failed to get Arc access token: %w", err)
	}

	// Create a mock access token structure with expiration
	// Arc tokens typically have a 1-hour expiration
	expirationTime := time.Now().Add(1 * time.Hour)

	return outputToken(out, ec, accessToken, expirationTime)
}

const execInfoEnv = "KUBERNETES_EXEC_INFO"

var scheme = runtime.NewScheme()
var codecs = serializer.NewCodecFactory(scheme)

func init() {
	install.Install(scheme)
}

func resolveExecCredentialFromEnv() (runtime.Object, error) {
	data := os.Getenv(execInfoEnv)

	if data == "" {
		// we allow the env var to be empty for local testing purposes
		return &clientauthenticationv1.ExecCredential{
			TypeMeta: metav1.TypeMeta{
				APIVersion: "client.authentication.k8s.io/v1",
				Kind:       "ExecCredential",
			},
		}, nil
	}

	obj, gvk, err := codecs.UniversalDeserializer().Decode([]byte(data), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	expectedGK := schema.GroupKind{
		Group: clientauthentication.SchemeGroupVersion.Group,
		Kind:  "ExecCredential",
	}
	if gvk.GroupKind() != expectedGK {
		return nil, fmt.Errorf(
			"invalid group/kind: wanted %s, got %s",
			expectedGK.String(),
			gvk.GroupKind().String(),
		)
	}

	return obj, nil
}

func outputToken(out io.Writer, ec runtime.Object, token string, expirationTime time.Time) error {
	expiration := metav1.NewTime(expirationTime)

	switch t := ec.(type) {
	case *clientauthenticationv1.ExecCredential:
		t.Status = &clientauthenticationv1.ExecCredentialStatus{
			ExpirationTimestamp: &expiration,
			Token:               token,
		}
	case *clientauthenticationv1beta1.ExecCredential:
		t.Status = &clientauthenticationv1beta1.ExecCredentialStatus{
			ExpirationTimestamp: &expiration,
			Token:               token,
		}
	default:
		return fmt.Errorf("unsupported exec credential type: %T", ec)
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")

	return enc.Encode(ec)
}
