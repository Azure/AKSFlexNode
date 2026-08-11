package release

import "testing"

func TestAgentBinaryArchiveMember(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		goos    string
		goarch  string
		want    string
		wantErr bool
	}{
		"amd64": {
			goos:   "linux",
			goarch: "amd64",
			want:   "aks-flex-node-linux-amd64",
		},
		"arm64": {
			goos:   "linux",
			goarch: "arm64",
			want:   "aks-flex-node-linux-arm64",
		},
		"unsupported operating system": {
			goos:    "windows",
			goarch:  "amd64",
			wantErr: true,
		},
		"unsupported architecture": {
			goos:    "linux",
			goarch:  "riscv64",
			wantErr: true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := AgentBinaryArchiveMember(tt.goos, tt.goarch)
			if (err != nil) != tt.wantErr {
				t.Fatalf("AgentBinaryArchiveMember() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("AgentBinaryArchiveMember() = %q, want %q", got, tt.want)
			}
		})
	}
}
