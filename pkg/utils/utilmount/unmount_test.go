//go:build linux

package utilmount

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMountsBelow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mountinfo  string
		root       string
		wantMounts []string
	}{
		{
			name: "finds mounts under /var/lib/kubelet",
			mountinfo: `22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw
100 22 0:50 / /var/lib/kubelet rw,relatime - ext4 /dev/sda2 rw
101 100 0:51 / /var/lib/kubelet/pods/abc/volumes/kubernetes.io~secret/token rw - tmpfs tmpfs rw
102 100 0:52 / /var/lib/kubelet/pods/def/volumes/kubernetes.io~configmap/cfg rw - tmpfs tmpfs rw
103 22 0:53 / /var/log rw,relatime - ext4 /dev/sda3 rw
`,
			root: "/var/lib/kubelet",
			wantMounts: []string{
				"/var/lib/kubelet",
				"/var/lib/kubelet/pods/abc/volumes/kubernetes.io~secret/token",
				"/var/lib/kubelet/pods/def/volumes/kubernetes.io~configmap/cfg",
			},
		},
		{
			name: "no mounts under target",
			mountinfo: `22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw
103 22 0:53 / /var/log rw,relatime - ext4 /dev/sda3 rw
`,
			root:       "/var/lib/kubelet",
			wantMounts: nil,
		},
		{
			name:       "empty mountinfo",
			mountinfo:  "",
			root:       "/var/lib/kubelet",
			wantMounts: nil,
		},
		{
			name: "does not match prefix-overlapping paths",
			mountinfo: `22 1 8:1 / / rw,relatime - ext4 /dev/sda1 rw
100 22 0:50 / /var/lib/kubelet rw - ext4 /dev/sda2 rw
101 22 0:51 / /var/lib/kubelet-extra rw - ext4 /dev/sda3 rw
`,
			root: "/var/lib/kubelet",
			wantMounts: []string{
				"/var/lib/kubelet",
			},
		},
		{
			name: "root with trailing slash is normalized",
			mountinfo: `100 22 0:50 / /var/lib/kubelet rw - ext4 /dev/sda2 rw
101 100 0:51 / /var/lib/kubelet/pods/abc rw - tmpfs tmpfs rw
`,
			root: "/var/lib/kubelet/",
			wantMounts: []string{
				"/var/lib/kubelet",
				"/var/lib/kubelet/pods/abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Write fake mountinfo to a temp file and override the package var.
			dir := t.TempDir()
			fakePath := filepath.Join(dir, "mountinfo")
			if err := os.WriteFile(fakePath, []byte(tt.mountinfo), 0600); err != nil {
				t.Fatalf("write fake mountinfo: %v", err)
			}

			orig := mountInfoPath
			mountInfoPath = fakePath
			t.Cleanup(func() { mountInfoPath = orig })

			got, err := mountsBelow(tt.root)
			if err != nil {
				t.Fatalf("mountsBelow() error: %v", err)
			}

			if len(got) != len(tt.wantMounts) {
				t.Fatalf("mountsBelow() returned %d mounts, want %d\ngot:  %v\nwant: %v",
					len(got), len(tt.wantMounts), got, tt.wantMounts)
			}

			for i := range got {
				if got[i] != tt.wantMounts[i] {
					t.Errorf("mount[%d] = %q, want %q", i, got[i], tt.wantMounts[i])
				}
			}
		})
	}
}

func TestMountsBelow_FileNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakePath := filepath.Join(dir, "nonexistent")

	orig := mountInfoPath
	mountInfoPath = fakePath
	t.Cleanup(func() { mountInfoPath = orig })

	_, err := mountsBelow("/var/lib/kubelet")
	if err == nil {
		t.Fatal("expected error for missing mountinfo file, got nil")
	}
}
