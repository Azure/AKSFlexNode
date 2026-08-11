package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/release"
	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeBinaryMode      = 0o755
	agentUpgradeMaxArchiveBytes = 256 << 20
	agentUpgradeMaxBinaryBytes  = 256 << 20
	agentUpgradeLockPath        = "/run/aks-flex-node-agent-upgrade.lock"
)

type agentUpgradePaths struct {
	BinaryPath   string
	BluePath     string
	GreenPath    string
	CurrentPath  string
	LastGoodPath string
	SignalPath   string
}

func (p agentUpgradePaths) layout() agentbinary.Layout {
	return agentbinary.Layout{
		BinaryPath:   p.BinaryPath,
		BluePath:     p.BluePath,
		GreenPath:    p.GreenPath,
		CurrentPath:  p.CurrentPath,
		LastGoodPath: p.LastGoodPath,
	}
}

func (p agentUpgradePaths) sharedPaths() (goalstates.AgentUpgradePaths, error) {
	paths := goalstates.AgentUpgradePaths{
		BinaryPath:   p.BinaryPath,
		BluePath:     p.BluePath,
		GreenPath:    p.GreenPath,
		CurrentPath:  p.CurrentPath,
		LastGoodPath: p.LastGoodPath,
		SignalPath:   p.SignalPath,
	}
	target, err := filepath.EvalSymlinks(p.CurrentPath)
	if err == nil {
		paths.CurrentTargetPath = target
		return paths, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		paths.CurrentTargetPath = p.BinaryPath
		return paths, nil
	}
	return goalstates.AgentUpgradePaths{}, fmt.Errorf("resolve current agent binary: %w", err)
}

// ensureAgentUpgradeLayout adds Flex-specific ownership validation around the
// shared idempotent migration and link initialization implementation.
func ensureAgentUpgradeLayout(ctx context.Context, log *slog.Logger, paths agentUpgradePaths) error {
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
	sharedPaths, err := paths.sharedPaths()
	if err != nil {
		return err
	}
	if err := agentbinary.EnsureDaemonBinaryLinks(ctx, log, sharedPaths); err != nil {
		return err
	}
	if productionPaths {
		return validateRootOwnedAgentUpgradePaths(paths)
	}
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

// secureAgentInstallOptions intentionally follows the merged Unbounded
// MachineOperation contract: HTTP transport and an omitted digest are allowed
// when the control plane trusts the archive source and transport path. Flex
// still bounds the archive, requires one exact member, and verifies the
// candidate executable. Production callers should supply HTTPS and SHA-256.
func secureAgentInstallOptions(rawURL, expectedDigest string) (agentbinary.InstallOptions, error) {
	parsedURL, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil || parsedURL.Scheme != "http" && parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.Fragment != "" {
		return agentbinary.InstallOptions{}, fmt.Errorf("download URL must use HTTP or HTTPS, include a host, omit user information, and omit fragments")
	}
	digest := strings.TrimPrefix(strings.TrimSpace(expectedDigest), "sha256:")
	if digest != "" {
		decodedDigest, decodeErr := hex.DecodeString(digest)
		if decodeErr != nil || len(decodedDigest) != sha256.Size {
			return agentbinary.InstallOptions{}, fmt.Errorf("expected SHA-256 must be exactly 64 hexadecimal characters")
		}
	}
	member, err := release.AgentBinaryArchiveMember(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return agentbinary.InstallOptions{}, err
	}
	return agentbinary.InstallOptions{
		DownloadURL:       rawURL,
		ExpectedSHA256:    expectedDigest,
		ExpectedMember:    member,
		Mode:              agentUpgradeBinaryMode,
		MaxArchiveBytes:   agentUpgradeMaxArchiveBytes,
		MaxExtractedBytes: agentUpgradeMaxBinaryBytes,
		ExactMember:       true,
		HTTPClient:        &http.Client{Timeout: 10 * time.Minute},
	}, nil
}

func installAndSwitchAgentBinary(ctx context.Context, log *slog.Logger, rawURL, expectedDigest string, paths agentUpgradePaths) error {
	opts, err := secureAgentInstallOptions(rawURL, expectedDigest)
	if err != nil {
		return err
	}
	_, err = agentbinary.InstallAndSwitchFromTarGz(ctx, log, paths.layout(), opts)
	return err
}
