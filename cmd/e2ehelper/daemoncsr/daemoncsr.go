package daemoncsr

import (
	"context"
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
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
	secret, err := kubeClient.CoreV1().Secrets("kube-system").Get(ctx, "bootstrap-token-"+tokenID, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("get bootstrap token secret: %w", err)
	}
	return secret.Labels[e2eBootstrapLabel] == "true", nil
}
