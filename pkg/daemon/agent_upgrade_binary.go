package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
)

const (
	agentUpgradeBinaryMode      = 0o755
	agentUpgradeMaxArchiveBytes = 256 << 20
	agentUpgradeMaxBinaryBytes  = 256 << 20
)

type agentUpgradePaths struct {
	BinaryPath   string
	BluePath     string
	GreenPath    string
	CurrentPath  string
	LastGoodPath string
	SignalPath   string
}

// ensureAgentUpgradeLayout migrates a legacy direct binary into the blue slot.
// It is intentionally idempotent because bootstrap and daemon startup may both
// call it while converging an older installation.
func ensureAgentUpgradeLayout(ctx context.Context, log *slog.Logger, paths agentUpgradePaths) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if log == nil {
		return fmt.Errorf("logger is nil")
	}
	if err := validateAgentUpgradePaths(paths); err != nil {
		return err
	}
	productionPaths := paths == defaultAgentUpgradePaths()
	if productionPaths && os.Geteuid() != 0 {
		return fmt.Errorf("agent binary layout must be initialized as root")
	}
	if err := os.MkdirAll(filepath.Dir(paths.BluePath), 0o750); err != nil {
		return fmt.Errorf("create agent binary slot directory: %w", err)
	}

	currentTarget, err := resolvedExecutable(paths.CurrentPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve current agent binary: %w", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		seed, seedErr := initialAgentBinary(paths)
		if seedErr != nil {
			return fmt.Errorf("find initial agent binary: %w", seedErr)
		}
		if seed == paths.BinaryPath {
			if err := copyExecutable(paths.BinaryPath, paths.BluePath); err != nil {
				return fmt.Errorf("migrate legacy agent binary: %w", err)
			}
			seed = paths.BluePath
		}
		if err := replaceSymlink(paths.CurrentPath, seed); err != nil {
			return fmt.Errorf("initialize current agent symlink: %w", err)
		}
		currentTarget = seed
	}

	if _, err := resolvedExecutable(paths.LastGoodPath); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resolve last-good agent binary: %w", err)
		}
		if err := replaceSymlink(paths.LastGoodPath, currentTarget); err != nil {
			return fmt.Errorf("initialize last-good agent symlink: %w", err)
		}
	}

	binaryTarget, err := filepath.EvalSymlinks(paths.BinaryPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("resolve compatibility agent binary: %w", err)
	}
	if errors.Is(err, os.ErrNotExist) || binaryTarget != currentTarget {
		if err := replaceSymlink(paths.BinaryPath, paths.CurrentPath); err != nil {
			return fmt.Errorf("initialize compatibility agent symlink: %w", err)
		}
	}

	if productionPaths {
		if err := validateRootOwnedAgentUpgradePaths(paths); err != nil {
			return err
		}
	}
	log.Info("agent binary blue-green layout initialized", "current", paths.CurrentPath, "last_good", paths.LastGoodPath)
	return nil
}

func validateRootOwnedAgentUpgradePaths(paths agentUpgradePaths) error {
	for _, path := range []string{
		filepath.Dir(paths.BluePath),
		paths.BinaryPath,
		paths.BluePath,
		paths.GreenPath,
		paths.CurrentPath,
		paths.LastGoodPath,
		paths.SignalPath,
	} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect ownership of %s: %w", path, err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return fmt.Errorf("agent upgrade path %s is not root-owned", path)
		}
	}
	return nil
}

func validateAgentUpgradePaths(paths agentUpgradePaths) error {
	values := []string{paths.BinaryPath, paths.BluePath, paths.GreenPath, paths.CurrentPath, paths.LastGoodPath, paths.SignalPath}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("invalid agent upgrade path %q", value)
		}
		if _, ok := seen[value]; ok {
			return fmt.Errorf("duplicate agent upgrade path %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func initialAgentBinary(paths agentUpgradePaths) (string, error) {
	for _, path := range []string{paths.BluePath, paths.GreenPath, paths.BinaryPath} {
		if _, err := resolvedExecutable(path); err == nil {
			return path, nil
		}
	}
	return "", os.ErrNotExist
}

func resolvedExecutable(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not a regular executable file", path)
	}
	return resolved, nil
}

func copyExecutable(sourcePath, targetPath string) (err error) {
	source, err := os.Open(sourcePath) //nolint:gosec // paths are fixed daemon configuration
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	return utilio.InstallFileWithLimitedSize(targetPath, source, agentUpgradeBinaryMode, agentUpgradeMaxBinaryBytes)
}

func replaceSymlink(linkPath, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(linkPath), ".aks-flex-node-link-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	defer os.Remove(tempPath) //nolint:errcheck // best-effort cleanup before/after rename
	if err := os.Symlink(targetPath, tempPath); err != nil {
		return err
	}
	return os.Rename(tempPath, linkPath)
}

func expectedAgentArchiveMember() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return "aks-flex-node-linux-" + runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("unsupported agent upgrade architecture %q", runtime.GOARCH)
	}
}

func secureAgentInstallOptions(rawURL, expectedDigest string) (agentbinary.SecureInstallOptions, error) {
	member, err := expectedAgentArchiveMember()
	if err != nil {
		return agentbinary.SecureInstallOptions{}, err
	}
	opts := agentbinary.SecureInstallOptions{
		DownloadURL:       rawURL,
		ExpectedSHA256:    expectedDigest,
		ExpectedMember:    member,
		Mode:              agentUpgradeBinaryMode,
		MaxArchiveBytes:   agentUpgradeMaxArchiveBytes,
		MaxExtractedBytes: agentUpgradeMaxBinaryBytes,
	}
	if err := agentbinary.ValidateSecureInstallOptions(opts); err != nil {
		return agentbinary.SecureInstallOptions{}, err
	}
	return opts, nil
}

func installAndSwitchAgentBinary(ctx context.Context, log *slog.Logger, rawURL, expectedDigest string, paths agentUpgradePaths) error {
	opts, err := secureAgentInstallOptions(rawURL, expectedDigest)
	if err != nil {
		return err
	}
	layout := agentbinary.Layout{
		BinaryPath:   paths.BinaryPath,
		BluePath:     paths.BluePath,
		GreenPath:    paths.GreenPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
	}
	_, err = agentbinary.SecureInstallAndSwitch(ctx, log, layout, opts)
	return err
}
