package daemon

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	machinav1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = machinav1alpha3.AddToScheme(scheme)
	return scheme
}
