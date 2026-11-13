// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	executil "github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// UnmountBelowByPrefix unmounts at or below each prefix, deepest first. A failed
// unmount keeps that prefix's data; an unreadable mount table keeps all data.
func UnmountBelowByPrefix(ctx context.Context, log *slog.Logger, run none.Runner, mountInfo string, prefixes ...string) (map[string]error, error) {
	override := mountInfo != ""
	if !override {
		mountInfo = defaultMountInfo
	}

	f, err := openMountTable(mountInfo, override)
	if err != nil || f == nil {
		return nil, err
	}

	defer f.Close() //nolint:errcheck // read only

	type mount struct{ point, prefix string }

	var mounts []mount

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}

		mp := unescapeMount(fields[4])
		for _, p := range prefixes {
			if mp == p || strings.HasPrefix(mp, p+"/") {
				mounts = append(mounts, mount{point: mp, prefix: p})
			}
		}
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].point) > len(mounts[j].point) })

	if run == nil {
		run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
			return executil.OutputCmd(ctx, log, name, args...)
		}
	}

	failed := map[string]error{}

	for _, m := range mounts {
		if _, err := run(ctx, log, "umount", "-l", m.point); err != nil {
			failed[m.prefix] = errors.Join(failed[m.prefix], fmt.Errorf("unmount %s: %w", m.point, err))
		}
	}

	return failed, nil
}

// defaultMountInfo is the mount table of the agent's mount namespace.
const defaultMountInfo = "/proc/self/mountinfo"

// openMountTable opens the mount table at path. The real mount table always
// exists: without it nothing says which mounts are below a directory, and
// removing the directory could delete through one, so its absence is an error.
// A path a caller supplied (a test's) that does not exist means "no mounts",
// which is a nil file and no error.
func openMountTable(path string, override bool) (*os.File, error) {
	f, err := os.Open(path) //nolint:gosec // fixed procfs mount table or a caller-owned test fixture
	if errors.Is(err, os.ErrNotExist) && override {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read the mount table: %w", err)
	}

	return f, nil
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces and tabs.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
