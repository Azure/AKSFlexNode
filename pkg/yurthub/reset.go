package yurthub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// ResetHost removes none-mode Hub state before worker identity replacement or
// host teardown. It must not be called for a soft restart.
func ResetHost(ctx context.Context, worker Worker) error {
	if worker.Mode != isolation.ModeNone || worker.Root == nil || worker.Exec == nil || !validKubeletUnit(worker.KubeletUnit) {
		return fmt.Errorf("host hub reset requires a none worker root, executor and kubelet unit")
	}
	root, err := os.OpenRoot(worker.Root.Path("/"))
	if err != nil {
		return fmt.Errorf("open host root for hub reset: %w", err)
	}
	defer root.Close() //nolint:errcheck // directory handle
	return resetHost(ctx, root, worker)
}

func resetHost(ctx context.Context, root *os.Root, worker Worker) error {
	// Keep one existing ownership marker until sensitive state and links are gone.
	marker := ""
	for _, path := range []string{envFile, servicePath, startedFingerprint} {
		if _, err := root.Lstat(path); err == nil {
			marker = path
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect hub ownership marker %s: %w", path, err)
		}
	}
	dropIn := routingDropInFor(worker.KubeletUnit)
	if marker == "" {
		if _, err := root.Lstat(dropIn); err == nil {
			marker = dropIn
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect hub routing: %w", err)
		}
	}
	if marker == "" {
		// A journal alone can precede Hub mutation; don't infer ownership of
		// /var/lib/yurthub. Reload also finishes a retry after the last unit
		// marker was removed but systemd reload failed.
		if _, err := root.Lstat(hubWorkerJournal); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("inspect hub journal: %w", err)
		}
		if _, err := worker.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after host hub cleanup: %w", err)
		}
		if err := root.Remove(hubWorkerJournal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove hub journal: %w", err)
		}
		return nil
	}

	if err := restoreHostRouting(ctx, root, worker, marker == dropIn); err != nil {
		return err
	}
	if err := stopHostHub(ctx, root, worker); err != nil {
		return err
	}
	if err := root.RemoveAll("var/lib/yurthub"); err != nil {
		return fmt.Errorf("remove host hub identity and cache: %w", err)
	}
	if err := removeHostHubLinks(root); err != nil {
		return err
	}
	for _, path := range []string{servicePath, envFile, startedFingerprint} {
		if path == marker {
			continue
		}
		if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove hub marker %s: %w", path, err)
		}
	}
	if marker != servicePath && marker != dropIn {
		if _, err := worker.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after host hub cleanup: %w", err)
		}
	}
	if err := root.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove hub marker %s: %w", marker, err)
	}
	if marker == servicePath || marker == dropIn {
		if _, err := worker.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
			return fmt.Errorf("reload systemd after host hub cleanup: %w", err)
		}
	}
	if err := root.Remove(hubWorkerJournal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove hub journal: %w", err)
	}
	return nil
}

func restoreHostRouting(ctx context.Context, root *os.Root, worker Worker, keepDropIn bool) error {
	var removed bool
	if keepDropIn {
		dropIn := routingDropInFor(worker.KubeletUnit)
		// Keep the only ownership marker while switching kubelet to direct routing.
		if err := checkRoutingOwnership(root, dropIn); err != nil {
			return err
		}
		if err := utilio.WriteFileInRoot(root, dropIn, directRoutingUnit, 0o644); err != nil {
			return fmt.Errorf("restore direct kubelet routing: %w", err)
		}
		removed = true
	} else {
		var err error
		removed, err = restoreRoutingInRoot(root, worker.KubeletUnit)
		if err != nil {
			return err
		}
	}
	state, err := worker.Exec(ctx, "systemctl", "show", "--property=ActiveState", "--value", worker.KubeletUnit)
	if err != nil {
		return fmt.Errorf("inspect host kubelet before hub cleanup: %w", err)
	}
	switch state = strings.TrimSpace(state); state {
	case "active":
		if removed || expectKubeletArg(ctx, worker, directKubeconfigArg) != nil {
			if err := restartKubelet(ctx, worker); err != nil {
				return err
			}
			if err := expectKubeletArg(ctx, worker, directKubeconfigArg); err != nil {
				return err
			}
		}
	case "inactive", "failed":
	default:
		return fmt.Errorf("host kubelet is %q during hub cleanup", state)
	}
	return nil
}

func stopHostHub(ctx context.Context, root *os.Root, worker Worker) error {
	if _, err := root.Lstat(servicePath); err == nil {
		if _, err := worker.Exec(ctx, "systemctl", "disable", "--now", serviceName); err != nil {
			return fmt.Errorf("stop host hub: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect host hub service: %w", err)
	} else {
		state, err := worker.Exec(ctx, "systemctl", "show", "--property=ActiveState", "--value", serviceName)
		if err != nil {
			return fmt.Errorf("check host hub state: %w", err)
		}
		if state = strings.TrimSpace(state); state != "inactive" && state != "failed" {
			return fmt.Errorf("host hub unit file is missing but the service is %q", state)
		}
	}
	return nil
}

func removeHostHubLinks(root *os.Root) error {
	for _, link := range [][2]string{{hubKubeconfigLink, hubKubeconfigDest}, {hubCALink, hubCADest}} {
		info, err := root.Lstat(link[0])
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect hub link %s: %w", link[0], err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := root.Readlink(link[0])
		if err != nil {
			return fmt.Errorf("read hub link %s: %w", link[0], err)
		}
		if target == link[1] {
			if err := root.Remove(link[0]); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove hub link %s: %w", link[0], err)
			}
		}
	}
	return nil
}
