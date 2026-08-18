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

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	"github.com/go-logr/logr"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

const (
	DefaultListenAddress             = ":8080"
	DefaultShutdownTimeout           = 15 * time.Second
	DefaultBootstrapGroup            = "system:bootstrappers:aks-flex-node"
	DefaultDaemonGroup               = "aks-flex-node-daemons"
	DefaultMachineConfigMapNamespace = "kube-system"
	DefaultMachineConfigMapName      = "aks-flex-machines"
)

type MachineStore interface {
	Get(ctx context.Context, machineName string) (armcontainerservice.Machine, error)
}

type MachineNotFoundError struct {
	Name string
}

func (e *MachineNotFoundError) Error() string {
	if e == nil || e.Name == "" {
		return "machine not found"
	}
	return fmt.Sprintf("machine %q not found", e.Name)
}

type Options struct {
	ListenAddress             string
	ShutdownTimeout           time.Duration
	Kubeconfig                string
	MachineConfigMapNamespace string
	MachineConfigMapName      string
	EnableCSRApprover         bool
	BootstrapGroup            string
	DaemonGroup               string
	MaxExpirationSeconds      int32
}

type Server struct {
	log      *slog.Logger
	machines MachineStore
}

type configMapMachineStore struct {
	kubeClient kubernetes.Interface
	namespace  string
	name       string
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
	cfg, err := kubernetesRESTConfig(opts.Kubeconfig)
	if err != nil {
		return fmt.Errorf("build Kubernetes config: %w", err)
	}
	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	store := NewConfigMapMachineStore(kubeClient, effectiveMachineConfigMapNamespace(opts), effectiveMachineConfigMapName(opts))
	server := NewServer(log, store)

	var mgr ctrl.Manager
	if opts.EnableCSRApprover {
		mgr, err = setupCSRApprover(opts, log, server, cfg, kubeClient)
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
		log.Info(
			"starting aks-flex-controller",
			"listenAddress", opts.ListenAddress,
			"machineConfigMapNamespace", effectiveMachineConfigMapNamespace(opts),
			"machineConfigMapName", effectiveMachineConfigMapName(opts),
		)
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

	return waitForServerExit(ctx, httpServer, errCh, opts.ShutdownTimeout)
}

func waitForServerExit(ctx context.Context, httpServer *http.Server, errCh <-chan error, shutdownTimeout time.Duration) error {
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := httpServer.Shutdown(shutdownCtx)

	switch {
	case runErr != nil && shutdownErr != nil:
		return errors.Join(runErr, fmt.Errorf("shutdown aks-flex-controller: %w", shutdownErr))
	case runErr != nil:
		return runErr
	case shutdownErr != nil:
		return fmt.Errorf("shutdown aks-flex-controller: %w", shutdownErr)
	default:
		return nil
	}
}

func NewConfigMapMachineStore(kubeClient kubernetes.Interface, namespace, name string) MachineStore {
	return &configMapMachineStore{kubeClient: kubeClient, namespace: namespace, name: name}
}

func (s *configMapMachineStore) Get(ctx context.Context, machineName string) (armcontainerservice.Machine, error) {
	if s.kubeClient == nil {
		return armcontainerservice.Machine{}, fmt.Errorf("kubernetes client is nil")
	}
	configMap, err := s.kubeClient.CoreV1().ConfigMaps(s.namespace).Get(ctx, s.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return armcontainerservice.Machine{}, fmt.Errorf("machine config map %s/%s not found", s.namespace, s.name)
	}
	if err != nil {
		return armcontainerservice.Machine{}, fmt.Errorf("get machine config map %s/%s: %w", s.namespace, s.name, err)
	}
	for _, key := range []string{machineName + ".json", machineName} {
		if raw, ok := configMap.Data[key]; ok {
			var machine armcontainerservice.Machine
			if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &machine); err != nil {
				return armcontainerservice.Machine{}, fmt.Errorf("machine config map key %q is not valid ARM machine JSON: %w", key, err)
			}
			return machine, nil
		}
	}
	return armcontainerservice.Machine{}, &MachineNotFoundError{Name: machineName}
}

func NewServer(log *slog.Logger, machines MachineStore) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{log: log, machines: machines}
}

func setupCSRApprover(
	opts Options,
	log *slog.Logger,
	server *Server,
	cfg *rest.Config,
	kubeClient kubernetes.Interface,
) (ctrl.Manager, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
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

func kubernetesRESTConfig(kubeconfig string) (*rest.Config, error) {
	if strings.TrimSpace(kubeconfig) != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("create in-cluster config: %w", err)
	}
	return cfg, nil
}

func effectiveMachineConfigMapNamespace(opts Options) string {
	if opts.MachineConfigMapNamespace != "" {
		return opts.MachineConfigMapNamespace
	}
	return DefaultMachineConfigMapNamespace
}

func effectiveMachineConfigMapName(opts Options) string {
	if opts.MachineConfigMapName != "" {
		return opts.MachineConfigMapName
	}
	return DefaultMachineConfigMapName
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
	machineName, subresource, ok := parseMachinePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "InvalidMachineName", "machine name must be a single path segment")
		return
	}

	if subresource == "status" {
		s.handleMachineStatusMutation(w, r, machineName)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.handleMachineRead(w, r, machineName)
	case http.MethodPut, http.MethodPost:
		s.handleMachineMutation(w, r, machineName)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only GET, HEAD, PUT, and POST are supported")
	}
}

func parseMachinePath(requestPath string) (machineName string, subresource string, ok bool) {
	machinePath := strings.TrimPrefix(requestPath, "/machines/")
	machinePath = strings.Trim(machinePath, "/")
	parts := strings.Split(machinePath, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "status" {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func (s *Server) handleMachineRead(w http.ResponseWriter, r *http.Request, machineName string) {
	machine, err := s.machineJSON(r.Context(), machineName)
	if err != nil {
		s.writeMachineError(w, err)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, machine)
}

func (s *Server) handleMachineMutation(w http.ResponseWriter, r *http.Request, machineName string) {
	machine, err := s.machineJSON(r.Context(), machineName)
	if err != nil {
		s.writeMachineError(w, err)
		return
	}
	s.log.Debug("ignoring read-only machine mutation", "method", r.Method, "machine", machineName)
	writeJSON(w, http.StatusOK, machine)
}

func (s *Server) handleMachineStatusMutation(w http.ResponseWriter, r *http.Request, machineName string) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "only PATCH is supported for machine status")
		return
	}
	// The controller is backed by a read-only test/dev data source. Accepting and
	// ignoring status updates lets agents exercise the production mutation path
	// without granting this endpoint write access to the data source.
	s.log.Debug("ignoring read-only machine status mutation", "machine", machineName)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) machineJSON(ctx context.Context, machineName string) (armcontainerservice.Machine, error) {
	if s.machines == nil {
		return armcontainerservice.Machine{}, fmt.Errorf("machine store is not configured")
	}
	return s.machines.Get(ctx, machineName)
}

func (s *Server) writeMachineError(w http.ResponseWriter, err error) {
	var notFound *MachineNotFoundError
	if errors.As(err, &notFound) {
		writeError(w, http.StatusNotFound, "NotFound", err.Error())
		return
	}
	s.log.Error("machine config map read failed", "error", err)
	writeError(w, http.StatusServiceUnavailable, "MachineStoreUnavailable", "failed to read machine data source")
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
		return false, fmt.Errorf("machine store is not configured")
	}
	_, err := s.machines.Get(ctx, machineName)
	var notFound *MachineNotFoundError
	if errors.As(err, &notFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get machine %q: %w", machineName, err)
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
