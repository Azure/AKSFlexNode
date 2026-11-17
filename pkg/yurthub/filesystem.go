package yurthub

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// workerRoot keeps rooted I/O while following the SDK's shared kubelet and
// versioned binary mappings. A guest symlink cannot escape its filesystem root.
type workerRoot struct {
	root        *os.Root
	kubelet     *os.Root
	bin         *os.Root
	kubeletUnit string
}

func openWorkerRoot(view isolation.NodeRoot, kubeletUnit string) (*workerRoot, error) {
	base := view.Path("/")
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	r := &workerRoot{root: root, kubeletUnit: kubeletUnit}
	for _, region := range []struct {
		path string
		dest **os.Root
	}{
		{"/var/lib/kubelet", &r.kubelet}, {"/usr/local/bin", &r.bin},
	} {
		mapped := view.Path(region.path)
		if mapped == filepath.Join(base, region.path) {
			continue
		}
		*region.dest, err = os.OpenRoot(mapped)
		if err != nil {
			_ = r.Close()
			return nil, fmt.Errorf("open mapped worker path %s: %w", region.path, err)
		}
	}
	return r, nil
}
func (r *workerRoot) resolve(name string) (*os.Root, string) {
	name = strings.TrimPrefix(filepath.Clean("/"+name), "/")
	if r.kubeletUnit != "kubelet.service" {
		name = strings.Replace(name, "etc/systemd/system/kubelet.service.d/", "etc/systemd/system/"+r.kubeletUnit+".d/", 1)
	}
	for _, region := range []struct {
		prefix string
		root   *os.Root
	}{{"var/lib/kubelet", r.kubelet}, {"usr/local/bin", r.bin}} {
		if region.root != nil && (name == region.prefix || strings.HasPrefix(name, region.prefix+"/")) {
			rel := strings.TrimPrefix(strings.TrimPrefix(name, region.prefix), "/")
			if rel == "" {
				rel = "."
			}
			return region.root, rel
		}
	}
	return r.root, name
}
func (r *workerRoot) Close() error {
	errs := []error{r.root.Close()}
	if r.kubelet != nil {
		errs = append(errs, r.kubelet.Close())
	}
	if r.bin != nil {
		errs = append(errs, r.bin.Close())
	}
	return errors.Join(errs...)
}
func (r *workerRoot) ReadFile(name string) ([]byte, error) {
	root, path := r.resolve(name)
	return root.ReadFile(path)
}
func (r *workerRoot) Lstat(name string) (os.FileInfo, error) {
	root, path := r.resolve(name)
	return root.Lstat(path)
}
func (r *workerRoot) Readlink(name string) (string, error) {
	root, path := r.resolve(name)
	return root.Readlink(path)
}
func (r *workerRoot) Remove(name string) error {
	root, path := r.resolve(name)
	return root.Remove(path)
}
func (r *workerRoot) RemoveAll(name string) error {
	root, path := r.resolve(name)
	return root.RemoveAll(path)
}
func (r *workerRoot) MkdirAll(name string, mode os.FileMode) error {
	root, path := r.resolve(name)
	return root.MkdirAll(path, mode)
}
func (r *workerRoot) Chmod(name string, mode os.FileMode) error {
	root, path := r.resolve(name)
	return root.Chmod(path, mode)
}
func (r *workerRoot) OpenFile(name string, flag int, mode os.FileMode) (*os.File, error) {
	root, path := r.resolve(name)
	return root.OpenFile(path, flag, mode)
}
func (r *workerRoot) Symlink(target, name string) error {
	root, path := r.resolve(name)
	return root.Symlink(target, path)
}
func (r *workerRoot) WriteFile(name string, data []byte, mode os.FileMode) error {
	root, path := r.resolve(name)
	return utilio.WriteFileInRoot(root, path, data, mode)
}
