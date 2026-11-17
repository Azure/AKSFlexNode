package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/pkg/agent/isolation/container"
)

// SyncContainerFilesystems persists runtime and boot state before applied-state publication.
func SyncContainerFilesystems(ctx context.Context, node ContainerRuntime) error {
	paths, err := preparedContainerPaths(node)
	if err != nil {
		return err
	}
	return syncPreparedFilesystems(ctx, paths, syncContainerFilesystem)
}

func preparedContainerPaths(node ContainerRuntime) ([]string, error) {
	gs, err := node.GoalState()
	if err != nil {
		return nil, err
	}
	root, ok := gs.RootFS.Root().(container.Root)
	if !ok || !filepath.IsAbs(root.Dir) || root.Dir == "/" {
		return nil, fmt.Errorf("container has no private prepared root")
	}
	if _, err := os.Stat(root.Dir); err != nil {
		return nil, fmt.Errorf("inspect prepared container root: %w", err)
	}
	backend := node.Backend()
	systemdDir, runtimePath := backend.SystemdDir, backend.RuntimePath
	if systemdDir == "" {
		systemdDir = container.SystemdDir
	}
	if runtimePath == "" {
		runtimePath = container.DefaultRuntimePath
	}
	paths := []string{root.Dir, backend.Layout.BundleDir(root.SlotName), systemdDir,
		filepath.Dir(runtimePath), filepath.Dir(backend.HelperPath)}
	for _, mount := range container.SamePathMounts {
		paths = append(paths, root.Path(mount.Path))
	}
	return paths, nil
}

// A filesystem-wide barrier covers image files, renamed configuration and
// removed identities without a path allowlist that can drift from installers.
// Each touched filesystem is flushed once, including separate /var and /opt.
func syncPreparedFilesystems(ctx context.Context, paths []string, syncFS func(*os.File) error) error {
	seen := make(map[uint64]bool)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !filepath.IsAbs(path) {
			return fmt.Errorf("prepared filesystem path is not absolute: %s", path)
		}
		f, err := openPreparedPath(path)
		if err != nil {
			return fmt.Errorf("open prepared filesystem %s: %w", path, err)
		}
		info, err := f.Stat()
		if err != nil {
			return errors.Join(err, f.Close())
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.Join(fmt.Errorf("no filesystem identity for %s", path), f.Close())
		}
		if seen[stat.Dev] {
			if err := f.Close(); err != nil {
				return err
			}
			continue
		}
		seen[stat.Dev] = true
		if err := errors.Join(syncFS(f), f.Close()); err != nil {
			return fmt.Errorf("persist prepared filesystem %s: %w", path, err)
		}
	}
	return ctx.Err()
}

func openPreparedPath(path string) (*os.File, error) {
	for {
		f, err := os.Open(path) //nolint:gosec // paths are owned prepared-node locations
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(path) == path {
			return f, err
		}
		// An absent optional path or removed identity still needs its parent's
		// directory entries persisted before Flex completion is recorded.
		path = filepath.Dir(path)
	}
}

func syncContainerFilesystem(f *os.File) error {
	return unix.Syncfs(int(f.Fd()))
}
