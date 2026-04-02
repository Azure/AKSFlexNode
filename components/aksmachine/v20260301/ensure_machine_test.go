package v20260301

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/Azure/AKSFlexNode/components/aksmachine"
)

// ptrT is a test helper that returns a pointer to v.
func ptrT[T any](v T) *T { return &v }

func TestBuildARMClientOptions(t *testing.T) {
	t.Parallel()

	t.Run("empty override returns nil", func(t *testing.T) {
		t.Parallel()
		if got := buildARMClientOptions(""); got != nil {
			t.Fatalf("buildARMClientOptions(\"\") = %v, want nil", got)
		}
	})

	t.Run("non-empty override sets endpoint", func(t *testing.T) {
		t.Parallel()
		const endpoint = "http://localhost:8080"
		opts := buildARMClientOptions(endpoint)
		if opts == nil {
			t.Fatalf("buildARMClientOptions(%q) = nil, want non-nil", endpoint)
		}
		if len(opts.Cloud.Services) == 0 {
			t.Fatalf("no cloud services configured")
		}
		for _, v := range opts.Cloud.Services {
			if v.Endpoint != endpoint {
				t.Fatalf("endpoint=%q, want %q", v.Endpoint, endpoint)
			}
		}
	})
}

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "404 ResponseError", err: &azcore.ResponseError{StatusCode: http.StatusNotFound}, want: true},
		{name: "500 ResponseError", err: &azcore.ResponseError{StatusCode: http.StatusInternalServerError}, want: false},
		{name: "non-ResponseError", err: errors.New("something went wrong"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isNotFound(tc.err); got != tc.want {
				t.Fatalf("isNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCredentialFromSpec_Nil(t *testing.T) {
	t.Parallel()
	cred, err := credentialFromSpec(nil)
	if err == nil && cred == nil {
		t.Fatalf("expected non-nil credential or error for nil spec, got cred=nil, err=nil")
	}
}

func TestCredentialFromSpec_EmptyCredential(t *testing.T) {
	t.Parallel()
	empty := aksmachine.AzureCredential_builder{}.Build()
	cred, err := credentialFromSpec(empty)
	if err == nil && cred == nil {
		t.Fatalf("expected non-nil credential or error for empty credential spec, got cred=nil, err=nil")
	}
}

func TestBuildK8sProfile_EmptySpec(t *testing.T) {
	t.Parallel()
	spec := aksmachine.EnsureMachineSpec_builder{}.Build()
	p := buildK8sProfile(spec)
	if p == nil {
		t.Fatalf("profile is nil")
	}
	if p.OrchestratorVersion != nil {
		t.Fatalf("OrchestratorVersion should be nil for empty spec")
	}
	if p.MaxPods != nil {
		t.Fatalf("MaxPods should be nil for empty spec")
	}
	if len(p.NodeLabels) != 0 {
		t.Fatalf("NodeLabels should be empty for empty spec")
	}
	if len(p.NodeTaints) != 0 {
		t.Fatalf("NodeTaints should be empty for empty spec")
	}
	if p.KubeletConfig != nil {
		t.Fatalf("KubeletConfig should be nil for empty spec")
	}
}

func TestBuildK8sProfile_KubernetesVersion(t *testing.T) {
	t.Parallel()
	spec := aksmachine.EnsureMachineSpec_builder{KubernetesVersion: ptrT("1.30.6")}.Build()
	p := buildK8sProfile(spec)
	if p.OrchestratorVersion == nil || *p.OrchestratorVersion != "1.30.6" {
		t.Fatalf("OrchestratorVersion=%v, want 1.30.6", p.OrchestratorVersion)
	}
}

func TestBuildK8sProfile_MaxPods(t *testing.T) {
	t.Parallel()
	t.Run("positive value propagated", func(t *testing.T) {
		t.Parallel()
		spec := aksmachine.EnsureMachineSpec_builder{MaxPods: ptrT(int32(110))}.Build()
		p := buildK8sProfile(spec)
		if p.MaxPods == nil || *p.MaxPods != 110 {
			t.Fatalf("MaxPods=%v, want 110", p.MaxPods)
		}
	})
	t.Run("zero value not propagated", func(t *testing.T) {
		t.Parallel()
		spec := aksmachine.EnsureMachineSpec_builder{MaxPods: ptrT(int32(0))}.Build()
		p := buildK8sProfile(spec)
		if p.MaxPods != nil {
			t.Fatalf("MaxPods=%v, want nil for zero value", p.MaxPods)
		}
	})
}

func TestBuildK8sProfile_NodeLabels(t *testing.T) {
	t.Parallel()
	spec := aksmachine.EnsureMachineSpec_builder{
		NodeLabels: map[string]string{"env": "prod", "team": "infra"},
	}.Build()
	p := buildK8sProfile(spec)
	if len(p.NodeLabels) != 2 {
		t.Fatalf("NodeLabels len=%d, want 2", len(p.NodeLabels))
	}
	if v := p.NodeLabels["env"]; v == nil || *v != "prod" {
		t.Fatalf("NodeLabels[env]=%v, want prod", v)
	}
}

func TestBuildK8sProfile_NodeTaints(t *testing.T) {
	t.Parallel()
	spec := aksmachine.EnsureMachineSpec_builder{
		NodeTaints: []string{"key=value:NoSchedule"},
	}.Build()
	p := buildK8sProfile(spec)
	if len(p.NodeTaints) != 1 {
		t.Fatalf("NodeTaints len=%d, want 1", len(p.NodeTaints))
	}
	if p.NodeTaints[0] == nil || *p.NodeTaints[0] != "key=value:NoSchedule" {
		t.Fatalf("NodeTaints[0]=%v, want key=value:NoSchedule", p.NodeTaints[0])
	}
}

func TestBuildK8sProfile_NodeInitializationTaints(t *testing.T) {
	t.Parallel()
	spec := aksmachine.EnsureMachineSpec_builder{
		NodeInitializationTaints: []string{"init-key=init-val:NoExecute"},
	}.Build()
	p := buildK8sProfile(spec)
	if len(p.NodeInitializationTaints) != 1 {
		t.Fatalf("NodeInitializationTaints len=%d, want 1", len(p.NodeInitializationTaints))
	}
	if p.NodeInitializationTaints[0] == nil || *p.NodeInitializationTaints[0] != "init-key=init-val:NoExecute" {
		t.Fatalf("NodeInitializationTaints[0]=%v, want init-key=init-val:NoExecute", p.NodeInitializationTaints[0])
	}
}

func TestBuildK8sProfile_KubeletConfig(t *testing.T) {
	t.Parallel()
	t.Run("positive thresholds propagated", func(t *testing.T) {
		t.Parallel()
		kc := aksmachine.MachineKubeletConfig_builder{
			ImageGcHighThreshold: ptrT(int32(85)),
			ImageGcLowThreshold:  ptrT(int32(70)),
		}.Build()
		spec := aksmachine.EnsureMachineSpec_builder{KubeletConfig: kc}.Build()
		p := buildK8sProfile(spec)
		if p.KubeletConfig == nil {
			t.Fatalf("KubeletConfig is nil")
		}
		if p.KubeletConfig.ImageGcHighThreshold == nil || *p.KubeletConfig.ImageGcHighThreshold != 85 {
			t.Fatalf("ImageGcHighThreshold=%v, want 85", p.KubeletConfig.ImageGcHighThreshold)
		}
		if p.KubeletConfig.ImageGcLowThreshold == nil || *p.KubeletConfig.ImageGcLowThreshold != 70 {
			t.Fatalf("ImageGcLowThreshold=%v, want 70", p.KubeletConfig.ImageGcLowThreshold)
		}
	})
	t.Run("zero thresholds not propagated", func(t *testing.T) {
		t.Parallel()
		kc := aksmachine.MachineKubeletConfig_builder{
			ImageGcHighThreshold: ptrT(int32(0)),
			ImageGcLowThreshold:  ptrT(int32(0)),
		}.Build()
		spec := aksmachine.EnsureMachineSpec_builder{KubeletConfig: kc}.Build()
		p := buildK8sProfile(spec)
		if p.KubeletConfig == nil {
			t.Fatalf("KubeletConfig is nil")
		}
		if p.KubeletConfig.ImageGcHighThreshold != nil {
			t.Fatalf("ImageGcHighThreshold=%v, want nil", p.KubeletConfig.ImageGcHighThreshold)
		}
		if p.KubeletConfig.ImageGcLowThreshold != nil {
			t.Fatalf("ImageGcLowThreshold=%v, want nil", p.KubeletConfig.ImageGcLowThreshold)
		}
	})
}
