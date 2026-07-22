package nodebootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/renameio/v2"
)

const (
	defaultAgentPath    = "/usr/local/bin/aks-flex-node"
	maxAgentDownload    = int64(1 << 30) // 1 GiB
	maxExtractedAgent   = int64(1 << 30) // 1 GiB
	AgentUpdateGuardEnv = "AKS_FLEX_NODE_AGENT_UPDATE_APPLIED"
)

// AgentUpdateOptions configures an optional replacement of the baked agent.
type AgentUpdateOptions struct {
	Source         string
	SHA256         string
	Format         string
	Destination    string
	GOOS           string
	GOARCH         string
	UpdateGuardSet bool
}

// AgentUpdateResult describes whether the caller must re-execute the active
// agent to continue with the newly installed code.
type AgentUpdateResult struct {
	Path    string
	Updated bool
}

// UpdateAgent downloads, verifies, and atomically installs an agent update.
func UpdateAgent(ctx context.Context, downloader *Downloader, options AgentUpdateOptions) (AgentUpdateResult, error) {
	destination := strings.TrimSpace(options.Destination)
	if destination == "" {
		destination = defaultAgentPath
	}
	result := AgentUpdateResult{Path: destination}
	if strings.TrimSpace(options.Source) == "" || options.UpdateGuardSet {
		return result, nil
	}
	if downloader == nil {
		return result, fmt.Errorf("agent update downloader is required")
	}

	goos := options.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := options.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	source := expandArtifactSource(options.Source, goos, goarch)
	artifact, err := downloader.Fetch(ctx, source, maxAgentDownload)
	if err != nil {
		return result, fmt.Errorf("download agent update: %w", err)
	}
	if err := verifyArtifactDigest(artifact, options.SHA256); err != nil {
		return result, err
	}

	binary, err := agentBinaryFromArtifact(artifact, options.Format, goos, goarch)
	if err != nil {
		return result, err
	}
	if len(binary) == 0 {
		return result, fmt.Errorf("agent update binary is empty")
	}
	if sameFileDigest(destination, binary) {
		return result, nil
	}
	// #nosec G301 -- the active binary directory must be traversable by systemd services.
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return result, fmt.Errorf("create agent destination directory: %w", err)
	}
	if err := renameio.WriteFile(destination, binary, 0o755); err != nil {
		return result, fmt.Errorf("atomically install agent update: %w", err)
	}
	// #nosec G302 -- the installed artifact is an executable, not sensitive data.
	if err := os.Chmod(destination, 0o755); err != nil {
		return result, fmt.Errorf("set agent update permissions: %w", err)
	}
	result.Updated = true
	return result, nil
}

func expandArtifactSource(source, goos, goarch string) string {
	replacer := strings.NewReplacer(
		"{{OS}}", goos,
		"{{ARCH}}", goarch,
	)
	return replacer.Replace(source)
}

func verifyArtifactDigest(artifact []byte, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return nil
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("agent binary SHA-256 must contain 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("parse agent binary SHA-256: %w", err)
	}
	actual := sha256.Sum256(artifact)
	if hex.EncodeToString(actual[:]) != expected {
		return fmt.Errorf("agent binary SHA-256 mismatch")
	}
	return nil
}

func agentBinaryFromArtifact(artifact []byte, format, goos, goarch string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "auto":
		if len(artifact) >= 2 && artifact[0] == 0x1f && artifact[1] == 0x8b {
			return binaryFromTarGzip(artifact, goos, goarch)
		}
		return artifact, nil
	case "binary", "raw":
		return artifact, nil
	case "archive", "tar.gz", "tgz":
		return binaryFromTarGzip(artifact, goos, goarch)
	default:
		return nil, fmt.Errorf("unsupported agent binary format %q: expected auto, binary, or tar.gz", format)
	}
}

func binaryFromTarGzip(artifact []byte, goos, goarch string) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return nil, fmt.Errorf("open agent update gzip archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()

	tarReader := tar.NewReader(gzipReader)
	expectedName := "aks-flex-node-" + goos + "-" + goarch
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read agent update archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			continue
		}
		name := filepath.Base(header.Name)
		if name != expectedName && name != "aks-flex-node" {
			continue
		}
		if header.Size < 0 || header.Size > maxExtractedAgent {
			return nil, fmt.Errorf("agent update binary exceeds %d bytes", maxExtractedAgent)
		}
		binary, err := io.ReadAll(io.LimitReader(tarReader, maxExtractedAgent+1))
		if err != nil {
			return nil, fmt.Errorf("extract agent update binary: %w", err)
		}
		if int64(len(binary)) > maxExtractedAgent {
			return nil, fmt.Errorf("agent update binary exceeds %d bytes", maxExtractedAgent)
		}
		return binary, nil
	}
	return nil, fmt.Errorf("agent update archive does not contain %s or aks-flex-node", expectedName)
}

func sameFileDigest(path string, expected []byte) bool {
	current, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false
	}
	currentDigest := sha256.Sum256(current)
	expectedDigest := sha256.Sum256(expected)
	return currentDigest == expectedDigest
}
