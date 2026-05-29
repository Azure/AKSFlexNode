package daemoncsr

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsAuthorizedE2EBootstrapSecret(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)
	base := func() *corev1.Secret {
		return &corev1.Secret{
			Type: corev1.SecretTypeBootstrapToken,
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					e2eBootstrapLabel: "true",
				},
			},
			Data: map[string][]byte{
				"token-id":                       []byte("abc123"),
				"token-secret":                   []byte("secret"),
				"expiration":                     []byte(now.Add(time.Hour).Format(time.RFC3339)),
				"usage-bootstrap-authentication": []byte("true"),
				"auth-extra-groups":              []byte(defaultBootstrapGroup),
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
		"missing label": {
			mutate: func(secret *corev1.Secret) { delete(secret.Labels, e2eBootstrapLabel) },
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
			mutate: func(secret *corev1.Secret) { secret.Data["expiration"] = []byte(now.Add(-time.Second).Format(time.RFC3339)) },
		},
		"malformed expiration": {
			mutate: func(secret *corev1.Secret) { secret.Data["expiration"] = []byte("not-a-time") },
		},
	}

	for name, tt := range tests {
		name, tt := name, tt
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			secret := base()
			if tt.mutate != nil {
				tt.mutate(secret)
			}

			got := isAuthorizedE2EBootstrapSecret(secret, "abc123", defaultBootstrapGroup, now)
			if got != tt.want {
				t.Fatalf("isAuthorizedE2EBootstrapSecret = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestHasBootstrapGroup(t *testing.T) {
	t.Parallel()

	if !hasBootstrapGroup([]byte("system:bootstrappers:other, system:bootstrappers:aks-flex-node"), defaultBootstrapGroup) {
		t.Fatal("hasBootstrapGroup = false, want true")
	}
	if hasBootstrapGroup([]byte("system:bootstrappers:other"), defaultBootstrapGroup) {
		t.Fatal("hasBootstrapGroup = true, want false")
	}
}
