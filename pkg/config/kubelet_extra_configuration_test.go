package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestKubeletExtraConfiguration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		extra string
		want  map[string]any
	}{
		{name: "absent"},
		{name: "empty", extra: `{}`},
		{
			name:  "image pull settings",
			extra: `{"serializeImagePulls":false,"maxParallelImagePulls":4}`,
			want:  map[string]any{"serializeImagePulls": false, "maxParallelImagePulls": float64(4)},
		},
		{
			name:  "typed settings win",
			extra: `{"maxPods":1,"imageGCHighThresholdPercent":95,"imageGCLowThresholdPercent":90}`,
		},
		{
			name:  "nested logging",
			extra: `{"logging":{"format":"json","verbosity":9}}`,
			want:  map[string]any{"logging": map[string]any{"format": "json", "verbosity": 2}},
		},
		{
			name:  "nested reservations",
			extra: `{"systemReserved":{"cpu":"9","ephemeral-storage":"2Gi"},"kubeReserved":{"memory":"9Gi","pid":"1000"}}`,
			want: map[string]any{
				"systemReserved": map[string]any{"cpu": "50m", "memory": "100Mi", "ephemeral-storage": "2Gi"},
				"kubeReserved":   map[string]any{"cpu": "200m", "memory": "650Mi", "pid": "1000"},
			},
		},
		{
			name:  "untyped lists and nested objects",
			extra: `{"allowedUnsafeSysctls":["kernel.shm_rmid_forced"],"featureGates":{"Example":false}}`,
			want: map[string]any{
				"allowedUnsafeSysctls": []any{"kernel.shm_rmid_forced"},
				"featureGates":         map[string]any{"Example": false},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Node: NodeConfig{MaxPods: 110, Kubelet: KubeletConfig{
				Verbosity: 2, ImageGCHighThreshold: 85, ImageGCLowThreshold: 80,
				SystemReserved: map[string]string{"cpu": "50m", "memory": "100Mi"},
				KubeReserved:   map[string]string{"cpu": "200m", "memory": "650Mi"},
			}}}
			want := kubeletConfiguration(cfg)
			for key, value := range tt.want {
				want[key] = value
			}
			if tt.extra != "" {
				if err := json.Unmarshal([]byte(tt.extra), &cfg.Node.Kubelet.ExtraConfiguration); err != nil {
					t.Fatalf("unmarshal extra configuration: %v", err)
				}
			}
			before, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal before merge: %v", err)
			}
			for range 2 {
				got := ToAgentConfig(cfg, "kube1").Kubelet.Configuration
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("configuration = %#v, want %#v", got, want)
				}
			}
			after, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("marshal after merge: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("configuration merge mutated the loaded config")
			}
		})
	}
}

func TestKubeletExtraConfigurationValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		extra   map[string]any
		wantErr string
	}{
		{name: "image pull settings", extra: map[string]any{"serializeImagePulls": false, "maxParallelImagePulls": 4}},
		{name: "version specific fields are untyped", extra: map[string]any{"futureKubeletField": true}},
		{name: "authorization mode", extra: map[string]any{"authorization": map[string]any{"mode": "AlwaysAllow"}}, wantErr: "authorization.mode"},
		{name: "non JSON value", extra: map[string]any{"example": make(chan int)}, wantErr: "JSON-compatible"},
	}
	for _, field := range []string{"apiVersion", "kind", "authentication", "clusterDNS", "containerRuntimeEndpoint", "registerWithTaints", "rotateCertificates"} {
		tests = append(tests, struct {
			name    string
			extra   map[string]any
			wantErr string
		}{name: field, extra: map[string]any{field: nil}, wantErr: field})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Node: NodeConfig{Kubelet: KubeletConfig{ExtraConfiguration: tt.extra}}}
			cfg.setNodeDefaults()
			err := cfg.Node.Kubelet.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestKubeletExtraConfigurationTypedDefaultsWin(t *testing.T) {
	t.Parallel()
	cfg := &Config{Node: NodeConfig{Kubelet: KubeletConfig{ExtraConfiguration: map[string]any{
		"maxPods":                     1,
		"imageGCHighThresholdPercent": 95,
		"imageGCLowThresholdPercent":  90,
		"logging":                     map[string]any{"verbosity": 9},
	}}}}
	cfg.setNodeDefaults()
	baseline := cfg.DeepCopy()
	baseline.Node.Kubelet.ExtraConfiguration = nil
	got, err := json.Marshal(kubeletConfiguration(cfg))
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(kubeletConfiguration(baseline))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("configuration = %s, want typed defaults %s", got, want)
	}
}

func TestKubeletExtraConfigurationJSONAndCopy(t *testing.T) {
	t.Parallel()
	var cfg Config
	if err := json.Unmarshal([]byte(`{"node":{"kubelet":{"extraConfiguration":{"serializeImagePulls":false,"maxParallelImagePulls":4,"logging":{"format":"json"}}}}}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	copy := cfg.DeepCopy()
	if copy == nil || !reflect.DeepEqual(copy, &cfg) {
		t.Fatal("deep copy did not preserve extra configuration")
	}
	logging, ok := copy.Node.Kubelet.ExtraConfiguration["logging"].(map[string]any)
	if !ok {
		t.Fatal("copied logging configuration is not an object")
	}
	logging["format"] = "text"
	copy.Node.Kubelet.ExtraConfiguration["maxParallelImagePulls"] = float64(8)
	if reflect.DeepEqual(copy, &cfg) {
		t.Fatal("copy did not change")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"serializeImagePulls":false`) || !strings.Contains(string(data), `"maxParallelImagePulls":4`) || !strings.Contains(string(data), `"format":"json"`) {
		t.Fatalf("original extra configuration changed: %s", data)
	}
}
