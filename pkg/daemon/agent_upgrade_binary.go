package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/AKSFlexNode/pkg/utils/utilio"
)

const (
	agentUpgradeBinaryMode      = 0o755
	agentUpgradeMaxArchiveBytes = 256 << 20
	agentUpgradeMaxBinaryBytes  = 256 << 20
	agentUpgradeVerifyTimeout   = 30 * time.Second
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

func parseAgentUpgradeSHA256(value string) ([sha256.Size]byte, error) {
	var expected [sha256.Size]byte
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return expected, fmt.Errorf("expected SHA-256 must be exactly 64 hexadecimal characters")
	}
	copy(expected[:], decoded)
	return expected, nil
}

func validateAgentUpgradeURL(rawURL string) (*url.URL, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fmt.Errorf("invalid download URL")
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, fmt.Errorf("download URL must be an HTTPS URL without user information")
	}
	return parsed, nil
}

func redactedAgentUpgradeURL(parsed *url.URL) string {
	redacted := *parsed
	redacted.RawQuery = ""
	redacted.Fragment = ""
	return redacted.String()
}

func expectedAgentArchiveMember() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return "aks-flex-node-linux-" + runtime.GOARCH, nil
	default:
		return "", fmt.Errorf("unsupported agent upgrade architecture %q", runtime.GOARCH)
	}
}

// installAndSwitchAgentBinary downloads and verifies an archive before making
// either daemon symlink visible. The archive digest covers the compressed bytes.
func installAndSwitchAgentBinary(ctx context.Context, log *slog.Logger, rawURL, expectedDigest string, paths agentUpgradePaths) error {
	return installAndSwitchAgentBinaryWithClient(ctx, log, newAgentUpgradeHTTPClient(), rawURL, expectedDigest, paths)
}

func installAndSwitchAgentBinaryWithClient(ctx context.Context, log *slog.Logger, client *http.Client, rawURL, expectedDigest string, paths agentUpgradePaths) error {
	parsedURL, err := validateAgentUpgradeURL(rawURL)
	if err != nil {
		return err
	}
	expected, err := parseAgentUpgradeSHA256(expectedDigest)
	if err != nil {
		return err
	}
	member, err := expectedAgentArchiveMember()
	if err != nil {
		return err
	}
	currentTarget, err := resolvedExecutable(paths.CurrentPath)
	if err != nil {
		return fmt.Errorf("resolve current agent binary: %w", err)
	}
	targetPath := paths.BluePath
	if currentTarget == paths.BluePath {
		targetPath = paths.GreenPath
	}

	archivePath, err := downloadAgentUpgradeArchive(ctx, client, parsedURL, expected)
	if err != nil {
		return err
	}
	defer os.Remove(archivePath) //nolint:errcheck // temporary archive cleanup
	// The inactive slot may still be the target of last-good. Point last-good
	// at the verified running binary before replacing that slot.
	if err := replaceSymlink(paths.LastGoodPath, currentTarget); err != nil {
		return fmt.Errorf("protect current agent as last-good: %w", err)
	}
	if err := extractAgentUpgradeBinary(archivePath, member, targetPath); err != nil {
		return err
	}
	if err := verifyAgentBinary(ctx, targetPath); err != nil {
		return err
	}
	if err := replaceSymlink(paths.CurrentPath, targetPath); err != nil {
		return fmt.Errorf("update current agent symlink: %w", err)
	}
	log.Info("staged upgraded agent binary", "url", redactedAgentUpgradeURL(parsedURL), "previous", currentTarget, "current", targetPath)
	return nil
}

func newAgentUpgradeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("redirect to non-HTTPS URL is not allowed")
			}
			return nil
		},
	}
}

func downloadAgentUpgradeArchive(ctx context.Context, client *http.Client, parsedURL *url.URL, expected [sha256.Size]byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), http.NoBody)
	if err != nil {
		return "", fmt.Errorf("create agent archive request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("download agent archive from %s: %w", redactedAgentUpgradeURL(parsedURL), ctx.Err())
		}
		// Redirect targets and transport errors may contain credential-bearing
		// URLs, so do not propagate the transport's error text.
		return "", fmt.Errorf("download agent archive from %s failed", redactedAgentUpgradeURL(parsedURL))
	}
	defer resp.Body.Close() //nolint:errcheck // response body cleanup
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download agent archive from %s: HTTP status %d", redactedAgentUpgradeURL(parsedURL), resp.StatusCode)
	}
	if resp.ContentLength > agentUpgradeMaxArchiveBytes {
		return "", fmt.Errorf("agent archive exceeds %d-byte limit", agentUpgradeMaxArchiveBytes)
	}

	temp, err := os.CreateTemp("", "aks-flex-node-upgrade-*.tar.gz")
	if err != nil {
		return "", fmt.Errorf("create temporary agent archive: %w", err)
	}
	path := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	hasher := sha256.New()
	limited := io.LimitReader(resp.Body, agentUpgradeMaxArchiveBytes+1)
	n, err := io.Copy(io.MultiWriter(temp, hasher), limited)
	if err != nil {
		return "", fmt.Errorf("read agent archive: %w", err)
	}
	if n > agentUpgradeMaxArchiveBytes {
		return "", fmt.Errorf("agent archive exceeds %d-byte limit", agentUpgradeMaxArchiveBytes)
	}
	if !equalDigest(hasher.Sum(nil), expected[:]) {
		return "", fmt.Errorf("agent archive SHA-256 does not match expected digest")
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("close temporary agent archive: %w", err)
	}
	ok = true
	return path, nil
}

func equalDigest(actual, expected []byte) bool {
	if len(actual) != len(expected) {
		return false
	}
	var different byte
	for i := range actual {
		different |= actual[i] ^ expected[i]
	}
	return different == 0
}

func extractAgentUpgradeBinary(archivePath, expectedMember, targetPath string) (err error) {
	archive, err := os.Open(archivePath) //nolint:gosec // path is an internally created temporary file
	if err != nil {
		return fmt.Errorf("open agent archive: %w", err)
	}
	defer func() {
		if closeErr := archive.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fmt.Errorf("decompress agent archive: %w", err)
	}
	defer gz.Close() //nolint:errcheck // read errors are reported while extracting

	found := false
	// Bound total decompressed input as well as the selected member so gzip
	// bombs hidden in unrelated archive members cannot consume unbounded work.
	decompressed := &countingReader{reader: io.LimitReader(gz, 2*agentUpgradeMaxBinaryBytes+1)}
	tarReader := tar.NewReader(decompressed)
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			return fmt.Errorf("read agent archive: %w", nextErr)
		}
		if header.Name == "" || filepath.IsAbs(header.Name) || filepath.Clean(header.Name) != header.Name || strings.Contains(header.Name, `\`) || strings.HasPrefix(header.Name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("agent archive contains unsafe member name %q", header.Name)
		}
		if header.Name != expectedMember {
			return fmt.Errorf("agent archive contains unexpected member %q", header.Name)
		}
		if found {
			return fmt.Errorf("agent archive contains duplicate member %q", expectedMember)
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > agentUpgradeMaxBinaryBytes {
			return fmt.Errorf("agent archive member %q is not a valid bounded regular file", expectedMember)
		}
		if err := utilio.InstallFileWithLimitedSize(targetPath, tarReader, agentUpgradeBinaryMode, agentUpgradeMaxBinaryBytes); err != nil {
			return fmt.Errorf("install upgraded agent binary: %w", err)
		}
		found = true
	}
	if decompressed.count > 2*agentUpgradeMaxBinaryBytes {
		return fmt.Errorf("decompressed agent archive exceeds %d-byte limit", 2*agentUpgradeMaxBinaryBytes)
	}
	if !found {
		return fmt.Errorf("agent archive does not contain expected member %q", expectedMember)
	}
	return nil
}

type countingReader struct {
	reader io.Reader
	count  int64
}

func (r *countingReader) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.count += int64(n)
	return n, err
}

func verifyAgentBinary(ctx context.Context, path string) error {
	verifyCtx, cancel := context.WithTimeout(ctx, agentUpgradeVerifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(verifyCtx, path, "version") //nolint:gosec // fixed verified binary path and argument
	// Candidate output is untrusted and could expose host data in operation
	// status. Only the command's success is relevant to verification.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify upgraded agent binary: %w", err)
	}
	return nil
}
