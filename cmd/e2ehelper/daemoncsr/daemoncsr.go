package daemoncsr

import (
	"context"
	"fmt"
	"strings"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	defaultBootstrapGroup = "system:bootstrappers:aks-flex-node"
	defaultDaemonGroup    = "aks-flex-node-daemons"
	e2eBootstrapLabel     = "aks-flex-node/e2e-daemon-csr"
)

var (
	flagKubeconfig     string
	flagDaemonGroup    string
	flagBootstrapGroup string
)

var Command = &cobra.Command{
	Use:          "daemon-csr-approver",
	Short:        "Approve daemon-controller CSRs for local e2e tests.",
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return run(cmd.Context())
	},
}

func init() {
	Command.Flags().StringVar(&flagKubeconfig, "kubeconfig", "", "Path to kubeconfig")
	Command.Flags().StringVar(&flagDaemonGroup, "daemon-group", defaultDaemonGroup, "Daemon certificate group")
	Command.Flags().StringVar(&flagBootstrapGroup, "bootstrap-group", defaultBootstrapGroup, "Bootstrap requester group")
}

func run(ctx context.Context) error {
	cfg, err := clientcmd.BuildConfigFromFlags("", flagKubeconfig)
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	approver, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		DaemonGroup:    flagDaemonGroup,
		BootstrapGroup: flagBootstrapGroup,
		AuthorizeBootstrap: func(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, _ string) (bool, error) {
			return bootstrapSecretHasE2ELabel(ctx, kubeClient, csr)
		},
		AuthorizeRenewal: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return true, nil
		},
	})
	if err != nil {
		return fmt.Errorf("create CSR approver: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	reconciler, err := daemoncred.NewCSRApproverReconciler(mgr.GetClient(), kubeClient, approver)
	if err != nil {
		return fmt.Errorf("create CSR approver reconciler: %w", err)
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup CSR approver reconciler: %w", err)
	}
	return mgr.Start(ctx)
}

func bootstrapSecretHasE2ELabel(ctx context.Context, kubeClient kubernetes.Interface, csr *certificatesv1.CertificateSigningRequest) (bool, error) {
	tokenID, ok := strings.CutPrefix(csr.Spec.Username, daemoncred.BootstrapUserPrefix)
	if !ok || tokenID == "" {
		return false, nil
	}
	secret, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, "bootstrap-token-"+tokenID, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get bootstrap token secret: %w", err)
	}
	return isAuthorizedE2EBootstrapSecret(secret, tokenID, flagBootstrapGroup, time.Now()), nil
}

func isAuthorizedE2EBootstrapSecret(secret *corev1.Secret, tokenID, bootstrapGroup string, now time.Time) bool {
	if secret.Type != corev1.SecretTypeBootstrapToken {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(secret.Labels[e2eBootstrapLabel]), "true") {
		return false
	}
	if strings.TrimSpace(string(secret.Data["token-id"])) != tokenID {
		return false
	}
	if strings.TrimSpace(string(secret.Data["token-secret"])) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(string(secret.Data["usage-bootstrap-authentication"])), "true") {
		return false
	}
	if !hasBootstrapGroup(secret.Data["auth-extra-groups"], bootstrapGroup) {
		return false
	}
	return !isExpired(secret.Data["expiration"], now)
}

func hasBootstrapGroup(raw []byte, bootstrapGroup string) bool {
	for group := range strings.SplitSeq(string(raw), ",") {
		if strings.TrimSpace(group) == bootstrapGroup {
			return true
		}
	}
	return false
}

func isExpired(raw []byte, now time.Time) bool {
	expiresAtRaw := strings.TrimSpace(string(raw))
	if expiresAtRaw == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, expiresAtRaw)
	if err != nil {
		return true
	}
	return !now.Before(expiresAt)
}
