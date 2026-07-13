package flexcontroller

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func TestWaitForServerExitShutsDownHTTPServerAfterCleanComponentExit(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}

	httpServer := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
		ReadHeaderTimeout: time.Second,
	}
	t.Cleanup(func() { _ = httpServer.Close() })
	serveErrCh := make(chan error, 1)
	go func() {
		serveErrCh <- httpServer.Serve(listener)
	}()

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatalf("initial GET error = %v", err)
	}
	_ = response.Body.Close()

	componentErrCh := make(chan error, 1)
	componentErrCh <- nil
	if err := waitForServerExit(context.Background(), httpServer, componentErrCh, time.Second); err != nil {
		t.Fatalf("waitForServerExit() error = %v", err)
	}

	select {
	case err := <-serveErrCh:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("Serve() error = %v, want http.ErrServerClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not stop")
	}
}

func TestServerGetMachine(t *testing.T) {
	t.Parallel()

	store := &fakeMachineStore{machine: testMachine("node1", "1.34.0", "42")}
	server := NewServer(nil, store)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/machines/node1", nil)

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", recorder.Code, recorder.Body.String())
	}
	if store.machineName != "node1" {
		t.Fatalf("machineName = %q, want node1", store.machineName)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"name":"node1"`) ||
		!strings.Contains(body, `"eTag":"42"`) ||
		!strings.Contains(body, `"orchestratorVersion":"1.34.0"`) {
		t.Fatalf("body = %s, want ARM machine JSON", body)
	}
}

func TestServerMachineNotFound(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, &fakeMachineStore{err: &MachineNotFoundError{Name: "node1"}})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/machines/node1", nil)

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestServerMachineStoreUnavailable(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, &fakeMachineStore{err: errors.New("config map unavailable")})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/machines/node1", nil)

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

func TestServerRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		method string
		path   string
		want   int
	}{
		"delete": {
			method: http.MethodDelete,
			path:   "/machines/node1",
			want:   http.StatusMethodNotAllowed,
		},
		"missing name": {
			method: http.MethodGet,
			path:   "/machines/",
			want:   http.StatusBadRequest,
		},
		"nested path": {
			method: http.MethodGet,
			path:   "/machines/node1/extra",
			want:   http.StatusBadRequest,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := NewServer(nil, &fakeMachineStore{})
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)

			server.Handler().ServeHTTP(recorder, req)

			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}

func TestServerIgnoresMachineMutations(t *testing.T) {
	t.Parallel()

	store := &fakeMachineStore{machine: testMachine("node1", "1.34.0", "42")}
	server := NewServer(nil, store)

	putRecorder := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/machines/node1", strings.NewReader(`{"properties":{"kubernetes":{"orchestratorVersion":"1.35.0"}}}`))
	server.Handler().ServeHTTP(putRecorder, putReq)
	if putRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200, body %s", putRecorder.Code, putRecorder.Body.String())
	}
	if !strings.Contains(putRecorder.Body.String(), "node1") {
		t.Fatalf("PUT body = %s, want existing machine", putRecorder.Body.String())
	}
	if store.machineName != "node1" {
		t.Fatalf("PUT machineName = %q, want node1", store.machineName)
	}

	statusRecorder := httptest.NewRecorder()
	statusReq := httptest.NewRequest(http.MethodPatch, "/machines/node1/status", strings.NewReader(`{"properties":{"status":{"provisioningState":"Succeeded"}}}`))
	server.Handler().ServeHTTP(statusRecorder, statusReq)
	if statusRecorder.Code != http.StatusNoContent {
		t.Fatalf("PATCH status = %d, want 204, body %s", statusRecorder.Code, statusRecorder.Body.String())
	}
}

func TestConfigMapMachineStore(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultMachineConfigMapName,
			Namespace: DefaultMachineConfigMapNamespace,
		},
		Data: map[string]string{
			"node1.json": `{"name":"node1","properties":{"eTag":"42","kubernetes":{"orchestratorVersion":"1.34.0"}}}`,
		},
	})
	store := NewConfigMapMachineStore(client, DefaultMachineConfigMapNamespace, DefaultMachineConfigMapName)

	machine, err := store.Get(context.Background(), "node1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if machine.Name == nil || *machine.Name != "node1" {
		t.Fatalf("machine = %#v", machine)
	}
	if machine.Properties == nil || machine.Properties.ETag == nil || *machine.Properties.ETag != "42" {
		t.Fatalf("machine properties = %#v", machine.Properties)
	}
	_, err = store.Get(context.Background(), "node2")
	var notFound *MachineNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("Get() error = %v, want MachineNotFoundError", err)
	}
}

func TestConfigMapMachineStoreInvalidJSON(t *testing.T) {
	t.Parallel()

	client := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      DefaultMachineConfigMapName,
			Namespace: DefaultMachineConfigMapNamespace,
		},
		Data: map[string]string{
			"node1": `not-json`,
		},
	})
	store := NewConfigMapMachineStore(client, DefaultMachineConfigMapNamespace, DefaultMachineConfigMapName)

	_, err := store.Get(context.Background(), "node1")
	if err == nil || !strings.Contains(err.Error(), "not valid ARM machine JSON") {
		t.Fatalf("Get() error = %v, want invalid ARM machine JSON", err)
	}
}

func TestIsAuthorizedBootstrapSecret(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	base := func() *corev1.Secret {
		return &corev1.Secret{
			Type: corev1.SecretTypeBootstrapToken,
			Data: map[string][]byte{
				"token-id":                       []byte("abc123"),
				"token-secret":                   []byte("secret"),
				"expiration":                     []byte(now.Add(time.Hour).Format(time.RFC3339)),
				"usage-bootstrap-authentication": []byte("true"),
				"auth-extra-groups":              []byte(DefaultBootstrapGroup),
			},
		}
	}

	tests := map[string]struct {
		mutate func(*corev1.Secret)
		want   bool
	}{
		"valid": {want: true},
		"wrong type": {
			mutate: func(secret *corev1.Secret) { secret.Type = corev1.SecretTypeOpaque },
		},
		"wrong token id": {
			mutate: func(secret *corev1.Secret) { secret.Data["token-id"] = []byte("other") },
		},
		"missing token secret": {
			mutate: func(secret *corev1.Secret) { delete(secret.Data, "token-secret") },
		},
		"auth disabled": {
			mutate: func(secret *corev1.Secret) { secret.Data["usage-bootstrap-authentication"] = []byte("false") },
		},
		"missing bootstrap group": {
			mutate: func(secret *corev1.Secret) { secret.Data["auth-extra-groups"] = []byte("system:bootstrappers:other") },
		},
		"expired": {
			mutate: func(secret *corev1.Secret) {
				secret.Data["expiration"] = []byte(now.Add(-time.Second).Format(time.RFC3339))
			},
		},
		"malformed expiration": {
			mutate: func(secret *corev1.Secret) { secret.Data["expiration"] = []byte("not-a-time") },
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			secret := base()
			if tt.mutate != nil {
				tt.mutate(secret)
			}
			got := isAuthorizedBootstrapSecret(secret, "abc123", DefaultBootstrapGroup, now)
			if got != tt.want {
				t.Fatalf("isAuthorizedBootstrapSecret = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestAuthorizeBootstrapCSRRequiresBootstrapSecretAndMachine(t *testing.T) {
	t.Parallel()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: metav1.NamespaceSystem,
		},
		Type: corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":                       []byte("abc123"),
			"token-secret":                   []byte("secret"),
			"usage-bootstrap-authentication": []byte("true"),
			"auth-extra-groups":              []byte(DefaultBootstrapGroup),
		},
	}
	csr := &certificatesv1.CertificateSigningRequest{
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Username: daemoncred.BootstrapUserPrefix + "abc123",
		},
	}
	store := &fakeMachineStore{machine: testMachine("node1", "1.34.0", "42")}
	server := NewServer(nil, store)

	allowed, err := server.authorizeBootstrapCSR(context.Background(), fake.NewSimpleClientset(secret), csr, "node1", DefaultBootstrapGroup)
	if err != nil {
		t.Fatalf("authorizeBootstrapCSR() error = %v", err)
	}
	if !allowed {
		t.Fatal("authorizeBootstrapCSR() = false, want true")
	}
	if store.machineName != "node1" {
		t.Fatalf("machineName = %q, want node1", store.machineName)
	}
}

func TestMachineExistsMissingMachine(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, &fakeMachineStore{err: &MachineNotFoundError{Name: "node1"}})
	exists, err := server.machineExists(context.Background(), "node1")
	if err != nil {
		t.Fatalf("machineExists() error = %v", err)
	}
	if exists {
		t.Fatal("machineExists() = true, want false")
	}
}

type fakeMachineStore struct {
	machine     *armcontainerservice.Machine
	err         error
	machineName string
}

func (f *fakeMachineStore) Get(_ context.Context, machineName string) (armcontainerservice.Machine, error) {
	f.machineName = machineName
	if f.err != nil {
		return armcontainerservice.Machine{}, f.err
	}
	if f.machine == nil {
		return armcontainerservice.Machine{}, errors.New("missing fake machine")
	}
	return *f.machine, nil
}

func testMachine(name, kubernetesVersion, etag string) *armcontainerservice.Machine {
	return &armcontainerservice.Machine{
		Name: &name,
		Properties: &armcontainerservice.MachineProperties{
			ETag: &etag,
			Kubernetes: &armcontainerservice.MachineKubernetesProfile{
				OrchestratorVersion: &kubernetesVersion,
			},
		},
	}
}
