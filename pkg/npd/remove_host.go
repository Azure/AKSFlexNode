package npd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilexec"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// RemoveHost removes Flex-owned NPD artifacts from a host-native worker.
func RemoveHost(log *slog.Logger, root isolation.NodeRoot) phases.Task {
	return &removeHostTask{log: log, root: root, run: utilexec.OutputCmd}
}

type removeHostTask struct {
	log  *slog.Logger
	root isolation.NodeRoot
	run  func(context.Context, *slog.Logger, string, ...string) (string, error)
}

func (*removeHostTask) Name() string { return "remove-host-npd" }

func (t *removeHostTask) Do(ctx context.Context) error {
	unit := t.root.Path(filepath.Join("/etc/systemd/system", systemdUnitNPD))
	owned, err := t.ownsUnit(unit)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}

	if _, err := t.run(ctx, t.log, "systemctl", "disable", "--now", systemdUnitNPD); err != nil {
		return fmt.Errorf("disable NPD: %w", err)
	}

	// The unit is the ownership evidence for a retry if an earlier removal fails.
	for _, path := range []string{t.root.Path(npdBinaryPath), t.root.Path(npdConfigPath), unit} {
		if err := os.Remove(filepath.Clean(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove NPD artifact %s: %w", path, err)
		}
	}
	return nil
}

func (t *removeHostTask) ownsUnit(unit string) (bool, error) {
	info, err := os.Lstat(unit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat NPD unit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}

	// Avoid following a symlink or blocking on a FIFO if the unit changes after Lstat.
	file, err := os.OpenFile(unit, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // unit path is constructed from the node root
	if err != nil {
		return false, fmt.Errorf("open NPD unit: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only

	info, err = file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat opened NPD unit: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return false, fmt.Errorf("read NPD unit: %w", err)
	}

	// Match the rendered template, not mentions of the paths in another unit.
	pattern := strings.NewReplacer(
		regexp.QuoteMeta("{{.NPDBinaryPath}}"), regexp.QuoteMeta(t.root.Path(npdBinaryPath)),
		regexp.QuoteMeta("{{.NPDConfigPath}}"), regexp.QuoteMeta(npdConfigPath),
		regexp.QuoteMeta("{{.NodeName}}"), `[^[:space:]"]*`,
		regexp.QuoteMeta("{{.APIServerURL}}"), `[^"\r\n]*`,
		regexp.QuoteMeta("{{.KubeconfigPath}}"), `[^"\r\n]*`,
	).Replace(regexp.QuoteMeta(serviceTemplate))

	owned, err := regexp.MatchString(`\A`+pattern+`\z`, string(content))
	if err != nil {
		return false, fmt.Errorf("match NPD unit: %w", err)
	}
	return owned, nil
}
