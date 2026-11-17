package agentruntime

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/container"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestInstallBinaryUsesNodeRoot(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := binaryDigest(t, self)
	for _, mode := range []string{"nspawn", "container", "none"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var root isolation.NodeRoot = isolation.DirRoot{Dir: dir}
			dest := filepath.Join(dir, "usr/local/bin/aks-flex-node")
			switch mode {
			case "container":
				root = container.Root{SlotName: "kube1", Dir: dir, HostRoot: t.TempDir()}
			case "none":
				layout := none.Layout{Base: t.TempDir()}
				root = none.Root{HostRoot: dir, Layout: layout, Version: "v1"}
				dest = filepath.Join(layout.VersionDir("v1"), "bin/aks-flex-node")
			}
			task := installBinary(root)
			if task.Name() != "install-binary-in-rootfs" {
				t.Fatalf("task name = %q", task.Name())
			}
			for range 2 {
				if err := task.Do(t.Context()); err != nil {
					t.Fatal(err)
				}
				if got := binaryDigest(t, dest); !bytes.Equal(got, want) {
					t.Fatal("installed credential helper differs from the current executable")
				}
				info, err := os.Stat(dest)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0o750 {
					t.Fatalf("installed mode = %o, want 750", info.Mode().Perm())
				}
			}
		})
	}
}

func binaryDigest(t *testing.T, path string) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	return hash.Sum(nil)
}
