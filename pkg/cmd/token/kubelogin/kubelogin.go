package kubelogin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/kubelogin/pkg/token"
	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/pkg/apis/clientauthentication"
	"k8s.io/client-go/pkg/apis/clientauthentication/install"
	clientauthenticationv1 "k8s.io/client-go/pkg/apis/clientauthentication/v1"
	clientauthenticationv1beta1 "k8s.io/client-go/pkg/apis/clientauthentication/v1beta1"
)

const aksAADServerID = "6dae42f8-4368-4678-94ff-3960e28e3630"

var flagServerID string
var flagPopEnabled bool
var flagPopClaims string
var flagClientCertificateFile string

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
	Command.Flags().BoolVar(
		&flagPopEnabled, "pop-enabled", false,
		"Enable PoP (Proof of Possession) token authentication.",
	)
	Command.Flags().StringVar(
		&flagPopClaims, "pop-claims", "",
		"Comma-separated list of key=value claims to include in the PoP token (e.g., 'u=cluster-resource-id').",
	)
	Command.Flags().StringVar(
		&flagClientCertificateFile, "client-certificate-file", "",
		"Path to the service principal client certificate file.",
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
	tokOpts.IsPoPTokenEnabled = flagPopEnabled
	tokOpts.PoPTokenClaims = flagPopClaims
	if flagClientCertificateFile != "" {
		certificatePath, cleanup, err := prepareClientCertificate(flagClientCertificateFile)
		if err != nil {
			return err
		}
		defer cleanup()
		tokOpts.ClientCert = certificatePath
	}
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

func prepareClientCertificate(certificateFile string) (string, func(), error) {
	data, err := config.LoadServicePrincipalCredentialFile(certificateFile)
	if err != nil {
		return "", nil, fmt.Errorf("load client certificate file: %w", err)
	}
	certificates, privateKey, err := azidentity.ParseCertificates(data, nil)
	if err != nil {
		return "", nil, fmt.Errorf("parse client certificate file: %w", err)
	}
	normalizedPEM, err := config.MarshalClientCertificatePEM(certificates, privateKey)
	if err != nil {
		return "", nil, fmt.Errorf("normalize client certificate file: %w", err)
	}
	file, err := os.CreateTemp("", "aks-flex-node-client-certificate-*.pem")
	if err != nil {
		return "", nil, fmt.Errorf("create temporary client certificate: %w", err)
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err := file.WriteString(normalizedPEM); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write temporary client certificate: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close temporary client certificate: %w", err)
	}
	return file.Name(), cleanup, nil
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
