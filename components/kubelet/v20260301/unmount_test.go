//go:build linux

package v20260301

import (
	"path/filepath"
	"strings"
	"testing"
)

// extractMountPoints is a test helper that replicates the mount-point
// filtering logic from unmountBelow without calling syscall.Unmount.
// It reads a fake /proc/mounts file and returns matching mount points.
func extractMountPoints(t *testing.T, fakeMounts string, root string) []string {
	t.Helper()

	if !strings.HasSuffix(root, "/") {
		root += "/"
	}

	var mounts []string
	for _, line := range strings.Split(fakeMounts, "\n") {
		fields := strings.Split(line, " ")
		if len(fields) < 2 {
			continue
		}
		if strings.HasPrefix(fields[1], root) {
			mounts = append(mounts, fields[1])
		}
	}

	return mounts
}

func TestMountPointFiltering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		procMounts string
		root       string
		wantMounts []string
	}{
		{
			name: "finds child mounts under /var/lib/kubelet",
			procMounts: `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
/dev/sda1 / ext4 rw,relatime 0 0
tmpfs /var/lib/kubelet/pods/abc/volumes/kubernetes.io~secret/token tmpfs rw 0 0
tmpfs /var/lib/kubelet/pods/def/volumes/kubernetes.io~configmap/cfg tmpfs rw 0 0
/dev/sda3 /var/log ext4 rw,relatime 0 0
`,
			root: "/var/lib/kubelet",
			wantMounts: []string{
				"/var/lib/kubelet/pods/abc/volumes/kubernetes.io~secret/token",
				"/var/lib/kubelet/pods/def/volumes/kubernetes.io~configmap/cfg",
			},
		},
		{
			name: "skips the root directory itself",
			procMounts: `/dev/sda2 /var/lib/kubelet ext4 rw,relatime 0 0
tmpfs /var/lib/kubelet/pods/abc tmpfs rw 0 0
`,
			root: "/var/lib/kubelet",
			wantMounts: []string{
				"/var/lib/kubelet/pods/abc",
			},
		},
		{
			name: "no mounts under target",
			procMounts: `sysfs /sys sysfs rw 0 0
/dev/sda3 /var/log ext4 rw,relatime 0 0
`,
			root:       "/var/lib/kubelet",
			wantMounts: nil,
		},
		{
			name:       "empty proc mounts",
			procMounts: "",
			root:       "/var/lib/kubelet",
			wantMounts: nil,
		},
		{
			name: "does not match prefix-overlapping paths",
			procMounts: `/dev/sda2 /var/lib/kubelet ext4 rw 0 0
/dev/sda3 /var/lib/kubelet-extra ext4 rw 0 0
tmpfs /var/lib/kubelet/pods/abc tmpfs rw 0 0
`,
			root: "/var/lib/kubelet",
			wantMounts: []string{
				"/var/lib/kubelet/pods/abc",
			},
		},
		{
			name: "root with trailing slash is handled",
			procMounts: `tmpfs /var/lib/kubelet/pods/abc tmpfs rw 0 0
`,
			root: "/var/lib/kubelet/",
			wantMounts: []string{
				"/var/lib/kubelet/pods/abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := extractMountPoints(t, tt.procMounts, tt.root)

			if len(got) != len(tt.wantMounts) {
				t.Fatalf("got %d mounts, want %d\ngot:  %v\nwant: %v",
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

func TestUnmountBelow_FileNotExist(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fakePath := filepath.Join(dir, "nonexistent")

	orig := procMountsPath
	procMountsPath = fakePath
	t.Cleanup(func() { procMountsPath = orig })

	err := unmountBelow("/var/lib/kubelet")
	if err == nil {
		t.Fatal("expected error for missing proc mounts file, got nil")
	}
}
