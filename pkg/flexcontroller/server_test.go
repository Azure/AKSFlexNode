package flexcontroller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func TestServerGetMachine(t *testing.T) {
	t.Parallel()

	store := &fakeMachineStore{machine: json.RawMessage(`{"id":"machine-id","name":"node1","properties":{"settings":{"kubernetesVersion":"1.34.0"}}}`)}
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
	if !strings.Contains(recorder.Body.String(), "node1") {
		t.Fatalf("body = %s, want machine JSON", recorder.Body.String())
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

	store := &fakeMachineStore{machine: json.RawMessage(`{"name":"node1"}`)}
	server := NewServer(nil, store)

	putRecorder := httptest.NewRecorder()
	putReq := httptest.NewRequest(http.MethodPut, "/machines/node1", strings.NewReader(`{"properties":{"settings":{"kubernetesVersion":"1.35.0"}}}`))
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
			"node1.json": `{"name":"node1"}`,
		},
	})
	store := NewConfigMapMachineStore(client, DefaultMachineConfigMapNamespace, DefaultMachineConfigMapName)

	machine, err := store.Get(context.Background(), "node1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if string(machine) != `{"name":"node1"}` {
		t.Fatalf("machine = %s", machine)
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
	if err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("Get() error = %v, want invalid JSON", err)
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
	store := &fakeMachineStore{machine: json.RawMessage(`{"name":"node1"}`)}
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
	machine     json.RawMessage
	err         error
	machineName string
}

func (f *fakeMachineStore) Get(_ context.Context, machineName string) (json.RawMessage, error) {
	f.machineName = machineName
	if f.err != nil {
		return nil, f.err
	}
	if f.machine == nil {
		return nil, errors.New("missing fake machine")
	}
	return f.machine, nil
}
