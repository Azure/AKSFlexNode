package flexcontroller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v8"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func TestServerGetMachine(t *testing.T) {
	t.Parallel()

	getter := &fakeMachinesGetter{
		response: armcontainerservice.MachinesClientGetResponse{
			Machine: armcontainerservice.Machine{
				ID:   ptr("machine-id"),
				Name: ptr("node1"),
				Properties: &armcontainerservice.MachineProperties{
					Kubernetes: &armcontainerservice.MachineKubernetesProfile{
						OrchestratorVersion: ptr("1.34.0"),
					},
				},
			},
		},
	}
	server := NewServer(nil, getter, "rg1", "cluster1", "pool1")
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/machines/node1", nil)

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %s", recorder.Code, recorder.Body.String())
	}
	if getter.machineName != "node1" || getter.resourceGroup != "rg1" || getter.clusterName != "cluster1" || getter.agentPoolName != "pool1" {
		t.Fatalf("getter call = %#v", getter)
	}
	if !strings.Contains(recorder.Body.String(), "node1") {
		t.Fatalf("body = %s, want machine JSON", recorder.Body.String())
	}
}

func TestServerMachineNotFound(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, &fakeMachinesGetter{err: &azcore.ResponseError{StatusCode: http.StatusNotFound, ErrorCode: "NotFound"}}, "rg1", "cluster1", "pool1")
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/machines/node1", nil)

	server.Handler().ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", recorder.Code)
	}
}

func TestServerRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		method string
		path   string
		want   int
	}{
		"post": {
			method: http.MethodPost,
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

			server := NewServer(nil, &fakeMachinesGetter{}, "rg1", "cluster1", "pool1")
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)

			server.Handler().ServeHTTP(recorder, req)

			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.want)
			}
		})
	}
}

func TestParseManagedClusterResourceID(t *testing.T) {
	t.Parallel()

	id := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.ContainerService/managedClusters/cluster1"
	parsed, err := parseManagedClusterResourceID(id)
	if err != nil {
		t.Fatalf("parseManagedClusterResourceID() error = %v", err)
	}
	if parsed.SubscriptionID != "sub1" || parsed.ResourceGroupName != "rg1" || parsed.Name != "cluster1" {
		t.Fatalf("parsed = %#v", parsed)
	}
	_, err = parseManagedClusterResourceID("/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/virtualMachines/vm1")
	if err == nil {
		t.Fatal("parseManagedClusterResourceID() error = nil, want invalid type")
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
	getter := &fakeMachinesGetter{response: armcontainerservice.MachinesClientGetResponse{Machine: armcontainerservice.Machine{Name: ptr("node1")}}}
	server := NewServer(nil, getter, "rg1", "cluster1", "pool1")

	allowed, err := server.authorizeBootstrapCSR(context.Background(), fake.NewSimpleClientset(secret), csr, "node1", DefaultBootstrapGroup)
	if err != nil {
		t.Fatalf("authorizeBootstrapCSR() error = %v", err)
	}
	if !allowed {
		t.Fatal("authorizeBootstrapCSR() = false, want true")
	}
	if getter.machineName != "node1" {
		t.Fatalf("machineName = %q, want node1", getter.machineName)
	}
}

func TestMachineExistsMissingMachine(t *testing.T) {
	t.Parallel()

	server := NewServer(nil, &fakeMachinesGetter{err: &azcore.ResponseError{StatusCode: http.StatusNotFound}}, "rg1", "cluster1", "pool1")
	exists, err := server.machineExists(context.Background(), "node1")
	if err != nil {
		t.Fatalf("machineExists() error = %v", err)
	}
	if exists {
		t.Fatal("machineExists() = true, want false")
	}
}

type fakeMachinesGetter struct {
	response      armcontainerservice.MachinesClientGetResponse
	err           error
	resourceGroup string
	clusterName   string
	agentPoolName string
	machineName   string
}

func (f *fakeMachinesGetter) Get(_ context.Context, resourceGroupName string, resourceName string, agentPoolName string, machineName string, _ *armcontainerservice.MachinesClientGetOptions) (armcontainerservice.MachinesClientGetResponse, error) {
	f.resourceGroup = resourceGroupName
	f.clusterName = resourceName
	f.agentPoolName = agentPoolName
	f.machineName = machineName
	if f.err != nil {
		return armcontainerservice.MachinesClientGetResponse{}, f.err
	}
	if f.response.Machine.Name == nil && f.response.Machine.ID == nil && f.response.Machine.Properties == nil {
		return armcontainerservice.MachinesClientGetResponse{}, errors.New("missing fake response")
	}
	return f.response, nil
}

func ptr[T any](v T) *T {
	return &v
}
