package yurthub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// ResetHost restores direct routing before removing Hub identity on a none
// repave or reset. A soft restart never calls it.
func ResetHost(ctx context.Context, log *slog.Logger) error {
	root, err := os.OpenRoot("/")
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // directory handle
	run := func(ctx context.Context, args ...string) (string, error) {
		return utilexec.OutputCmd(ctx, log, args[0], args[1:]...)
	}
	return resetHost(ctx, &workerRoot{root: root, kubeletUnit: none.UnitKubelet}, run)
}

func resetHost(ctx context.Context, root *workerRoot, run func(context.Context, ...string) (string, error)) error {
	owned := false
	for _, path := range []string{servicePath, envFile, routingDropIn, startedFingerprint} {
		if _, err := root.Lstat(path); err == nil {
			owned = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !owned {
		return nil
	}
	removed, err := restoreRoutingInRoot(root)
	if err != nil {
		return err
	}
	state, stateErr := run(ctx, "systemctl", "is-active", none.UnitKubelet)
	if stateErr == nil {
		ops := &Operations{cfg: Config{KubeletUnit: none.UnitKubelet}, machineRun: run}
		if removed || ops.expectKubeletArg(ctx, directKubeconfigArg) != nil {
			if err := ops.restartKubelet(ctx); err != nil {
				return err
			}
			if err := ops.expectKubeletArg(ctx, directKubeconfigArg); err != nil {
				return err
			}
		}
	} else if state = strings.TrimSpace(state); state != "inactive" && state != "failed" {
		return fmt.Errorf("inspect host kubelet before Hub cleanup: %w", stateErr)
	}
	if _, err := root.Lstat(servicePath); err == nil {
		if _, err := run(ctx, "systemctl", "disable", "--now", serviceName); err != nil {
			return fmt.Errorf("stop host Hub: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{servicePath, envFile, startedFingerprint} {
		if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := root.RemoveAll("var/lib/yurthub"); err != nil {
		return err
	}
	for _, link := range [][2]string{{hubKubeconfigLink, hubKubeconfigDest}, {hubCALink, hubCADest}} {
		if target, err := root.Readlink(link[0]); err == nil && target == link[1] {
			if err := root.Remove(link[0]); err != nil {
				return err
			}
		}
	}
	if _, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	return nil
}
