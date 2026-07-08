package flexcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/AKSFlexNode/pkg/azclient"
	"github.com/Azure/AKSFlexNode/pkg/config"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	DefaultListenAddress   = ":8080"
	DefaultShutdownTimeout = 15 * time.Second
	DefaultBootstrapGroup  = "system:bootstrappers:aks-flex-node"
	DefaultDaemonGroup     = "aks-flex-node-daemons"
)

type MachinesGetter interface {
	Get(
		ctx context.Context,
		resourceGroupName string,
		resourceName string,
		agentPoolName string,
		machineName string,
		options *armcontainerservice.MachinesClientGetOptions,
	) (armcontainerservice.MachinesClientGetResponse, error)
}

type Options struct {
	ListenAddress           string
	ClusterResourceID       string
	AgentPoolName           string
	ResourceManagerEndpoint string
	ShutdownTimeout         time.Duration
	EnableCSRApprover       bool
	Kubeconfig              string
	BootstrapGroup          string
	DaemonGroup             string
	MaxExpirationSeconds    int32
}

type Server struct {
	log           *slog.Logger
	machines      MachinesGetter
	resourceGroup string
	clusterName   string
	agentPoolName string
}

func Run(ctx context.Context, opts Options, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if opts.ListenAddress == "" {
		opts.ListenAddress = DefaultListenAddress
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	server, err := NewARMServer(opts, log)
	if err != nil {
		return err
	}

	var mgr ctrl.Manager
	if opts.EnableCSRApprover {
		mgr, err = setupCSRApprover(opts, log, server)
		if err != nil {
			return err
		}
	}

	httpServer := &http.Server{
		Addr:              opts.ListenAddress,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		log.Info("starting aks-flex-controller", "listenAddress", opts.ListenAddress)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	if mgr != nil {
		go func() {
			log.Info("starting daemon CSR approver", "bootstrapGroup", effectiveBootstrapGroup(opts), "daemonGroup", effectiveDaemonGroup(opts))
			errCh <- mgr.Start(ctx)
		}()
	}

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown aks-flex-controller: %w", err)
		}
		return nil
	case err := <-errCh:
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
			defer cancel()
			_ = httpServer.Shutdown(shutdownCtx)
		}
		return err
	}
}

func NewARMServer(opts Options, log *slog.Logger) (*Server, error) {
	clusterID, err := parseManagedClusterResourceID(opts.ClusterResourceID)
	if err != nil {
		return nil, err
	}
	agentPoolName := strings.TrimSpace(opts.AgentPoolName)
	if agentPoolName == "" {
		return nil, fmt.Errorf("agent pool name is empty")
	}
	clientOpts := azureClientOptions(opts.ResourceManagerEndpoint)
	cred, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{ClientOptions: clientOpts})
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	client, err := armcontainerservice.NewMachinesClient(clusterID.SubscriptionID, cred, &arm.ClientOptions{ClientOptions: clientOpts})
	if err != nil {
		return nil, fmt.Errorf("create ARM machines client: %w", err)
	}
	return NewServer(log, client, clusterID.ResourceGroupName, clusterID.Name, agentPoolName), nil
}

func NewServer(log *slog.Logger, machines MachinesGetter, resourceGroupName, clusterName, agentPoolName string) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		log:           log,
		machines:      machines,
		resourceGroup: resourceGroupName,
		clusterName:   clusterName,
		agentPoolName: agentPoolName,
	}
}

func setupCSRApprover(opts Options, log *slog.Logger, server *Server) (ctrl.Manager, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", opts.Kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("build Kubernetes config for CSR approver: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client for CSR approver: %w", err)
	}
	bootstrapGroup := effectiveBootstrapGroup(opts)
	daemonGroup := effectiveDaemonGroup(opts)
	approver, err := daemoncred.NewCSRApprover(daemoncred.CSRApproverOptions{
		DaemonGroup:          daemonGroup,
		BootstrapGroup:       bootstrapGroup,
		MaxExpirationSeconds: opts.MaxExpirationSeconds,
		AuthorizeBootstrap: func(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, nodeName string) (bool, error) {
			return server.authorizeBootstrapCSR(ctx, kubeClient, csr, nodeName, bootstrapGroup)
		},
		AuthorizeRenewal: func(ctx context.Context, _ *certificatesv1.CertificateSigningRequest, nodeName string) (bool, error) {
			return server.machineExists(ctx, nodeName)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create daemon CSR approver: %w", err)
	}
	mgr, err := ctrl.NewManager(cfg, manager.Options{
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, fmt.Errorf("create CSR approver manager: %w", err)
	}
	reconciler, err := daemoncred.NewCSRApproverReconciler(mgr.GetClient(), kubeClient, approver)
	if err != nil {
		return nil, fmt.Errorf("create CSR approver reconciler: %w", err)
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		return nil, fmt.Errorf("setup CSR approver reconciler: %w", err)
	}
	return mgr, nil
}

func effectiveBootstrapGroup(opts Options) string {
	if opts.BootstrapGroup != "" {
		return opts.BootstrapGroup
	}
	return DefaultBootstrapGroup
}

func effectiveDaemonGroup(opts Options) string {
	if opts.DaemonGroup != "" {
		return opts.DaemonGroup
	}
	return DefaultDaemonGroup
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleHealthz)
	mux.HandleFunc("/machines/", s.handleMachine)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET is supported")
		return
	}
	machineName := strings.TrimPrefix(r.URL.Path, "/machines/")
	machineName = strings.Trim(machineName, "/")
	if machineName == "" || strings.Contains(machineName, "/") {
		writeError(w, http.StatusBadRequest, "InvalidMachineName", "machine name must be a single path segment")
		return
	}
	if s.machines == nil {
		writeError(w, http.StatusServiceUnavailable, "NotReady", "machine client is not configured")
		return
	}

	resp, err := s.machines.Get(r.Context(), s.resourceGroup, s.clusterName, s.agentPoolName, machineName, nil)
	if err != nil {
		s.writeARMError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp.Machine)
}

func (s *Server) writeARMError(w http.ResponseWriter, err error) {
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		status := responseErr.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		code := responseErr.ErrorCode
		if code == "" {
			code = http.StatusText(status)
		}
		writeError(w, status, code, responseErr.Error())
		return
	}
	s.log.Error("ARM machine request failed", "error", err)
	writeError(w, http.StatusBadGateway, "UpstreamError", "failed to read AKS machine")
}

func (s *Server) authorizeBootstrapCSR(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	csr *certificatesv1.CertificateSigningRequest,
	nodeName string,
	bootstrapGroup string,
) (bool, error) {
	tokenID, ok := strings.CutPrefix(csr.Spec.Username, daemoncred.BootstrapUserPrefix)
	if !ok || tokenID == "" {
		return false, nil
	}
	secret, err := kubeClient.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, "bootstrap-token-"+tokenID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get bootstrap token secret: %w", err)
	}
	if !isAuthorizedBootstrapSecret(secret, tokenID, bootstrapGroup, time.Now()) {
		return false, nil
	}
	return s.machineExists(ctx, nodeName)
}

func (s *Server) machineExists(ctx context.Context, machineName string) (bool, error) {
	if s.machines == nil {
		return false, fmt.Errorf("machine client is not configured")
	}
	_, err := s.machines.Get(ctx, s.resourceGroup, s.clusterName, s.agentPoolName, machineName, nil)
	if isARMNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get AKS machine %q: %w", machineName, err)
	}
	return true, nil
}

func isAuthorizedBootstrapSecret(secret *corev1.Secret, tokenID, bootstrapGroup string, now time.Time) bool {
	if secret == nil || secret.Type != corev1.SecretTypeBootstrapToken {
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

func isARMNotFound(err error) bool {
	var responseErr *azcore.ResponseError
	return errors.As(err, &responseErr) && responseErr.StatusCode == http.StatusNotFound
}

func parseManagedClusterResourceID(resourceID string) (*arm.ResourceID, error) {
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return nil, fmt.Errorf("cluster resource ID is empty")
	}
	parsed, err := arm.ParseResourceID(resourceID)
	if err != nil {
		return nil, fmt.Errorf("parse cluster resource ID: %w", err)
	}
	if !strings.EqualFold(parsed.ResourceType.Namespace, "Microsoft.ContainerService") || !strings.EqualFold(parsed.ResourceType.Type, "managedClusters") {
		return nil, fmt.Errorf("cluster resource ID type %q/%q is not Microsoft.ContainerService/managedClusters", parsed.ResourceType.Namespace, parsed.ResourceType.Type)
	}
	return parsed, nil
}

func azureClientOptions(resourceManagerEndpoint string) azcore.ClientOptions {
	cfg := &config.Config{}
	if strings.TrimSpace(resourceManagerEndpoint) != "" {
		cfg.Azure.ResourceManagerEndpointURL = strings.TrimRight(strings.TrimSpace(resourceManagerEndpoint), "/")
	}
	return azclient.ClientOptionsFromConfig(cfg)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusNoContent {
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
