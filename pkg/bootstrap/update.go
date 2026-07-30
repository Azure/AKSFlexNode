package bootstrap

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/renameio/v2"
)

const maxAgentArtifactBytes = int64(1 << 30)

type UpdateResult struct {
	Path      string
	Reexecute bool
}

func InstallAgent(ctx context.Context, options Options) (UpdateResult, error) {
	destination := filepath.Join(options.InstallDir, "aks-flex-node")
	result := UpdateResult{Path: destination}
	if os.Getenv(updateGuardEnvironment) == "1" {
		return result, nil
	}
	if options.AgentURL == "" && options.AgentVersion == "" {
		executable, err := os.Executable()
		if err != nil {
			return result, fmt.Errorf("resolve current executable: %w", err)
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return result, fmt.Errorf("resolve absolute executable path: %w", err)
		}
		absoluteDestination, err := filepath.Abs(destination)
		if err != nil {
			return result, fmt.Errorf("resolve absolute agent destination: %w", err)
		}
		if executable == absoluteDestination {
			return result, nil
		}
		binary, err := os.ReadFile(filepath.Clean(executable))
		if err != nil {
			return result, fmt.Errorf("read current executable: %w", err)
		}
		if err := installAgentBinary(options.InstallDir, destination, binary); err != nil {
			return result, err
		}
		result.Reexecute = true
		return result, nil
	}
	archiveName := fmt.Sprintf("aks-flex-node-linux-%s.tar.gz", runtime.GOARCH)
	source := options.AgentURL
	if source == "" {
		source = fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", defaultRepository, options.AgentVersion, archiveName)
	}
	source = strings.NewReplacer(
		"{{OS}}", runtime.GOOS,
		"{{ARCH}}", runtime.GOARCH,
		"{{VERSION}}", options.AgentVersion,
		"{{ARCHIVE_NAME}}", archiveName,
	).Replace(source)
	artifact, err := downloadArtifact(ctx, source, maxAgentArtifactBytes)
	if err != nil {
		return result, fmt.Errorf("download agent: %w", err)
	}
	if err := verifySHA256(artifact, options.AgentSHA256); err != nil {
		return result, err
	}
	binary, err := extractAgent(artifact, strings.TrimSuffix(archiveName, ".tar.gz"))
	if err != nil {
		return result, err
	}
	if err := installAgentBinary(options.InstallDir, destination, binary); err != nil {
		return result, err
	}
	result.Reexecute = true
	return result, nil
}

func installAgentBinary(installDir, destination string, binary []byte) error {
	if err := os.MkdirAll(installDir, 0o755); err != nil { //nolint:gosec // executable directory
		return fmt.Errorf("create agent install directory: %w", err)
	}
	if sameContent(destination, binary) {
		return nil
	}
	if err := renameio.WriteFile(destination, binary, 0o755); err != nil { //nolint:gosec // executable
		return fmt.Errorf("atomically install agent: %w", err)
	}
	if err := os.Chmod(destination, 0o755); err != nil { //nolint:gosec // executable
		return fmt.Errorf("set agent permissions: %w", err)
	}
	return nil
}

func downloadArtifact(ctx context.Context, source string, maxBytes int64) ([]byte, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse source URL")
	}
	if parsed.Scheme == "" || parsed.Scheme == "file" {
		path := source
		if parsed.Scheme == "file" {
			if parsed.Host != "" && parsed.Host != "localhost" {
				return nil, fmt.Errorf("file URL must not have a remote host")
			}
			path, err = url.PathUnescape(parsed.Path)
			if err != nil {
				return nil, fmt.Errorf("decode file URL: %w", err)
			}
		}
		file, err := os.Open(filepath.Clean(path))
		if err != nil {
			return nil, fmt.Errorf("open local source: %w", err)
		}
		data, readErr := readLimited(file, maxBytes)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close local source: %w", closeErr)
		}
		return data, nil
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("unsupported source scheme %q", parsed.Scheme)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create download request")
	}
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("refusing HTTPS downgrade redirect")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request failed")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, fmt.Errorf("source returned HTTP status %d", response.StatusCode)
	}
	data, readErr := readLimited(response.Body, maxBytes)
	closeErr := response.Body.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close response: %w", closeErr)
	}
	return data, nil
}

func readLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func verifySHA256(data []byte, expected string) error {
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return nil
	}
	if len(expected) != sha256.Size*2 {
		return fmt.Errorf("agent SHA-256 must have 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(expected); err != nil {
		return fmt.Errorf("parse agent SHA-256: %w", err)
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expected {
		return fmt.Errorf("agent SHA-256 mismatch")
	}
	return nil
}

func extractAgent(artifact []byte, expectedName string) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return nil, fmt.Errorf("open agent archive: %w", err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read agent archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			continue
		}
		name := filepath.Base(header.Name)
		if name != expectedName && name != "aks-flex-node" {
			continue
		}
		if header.Size <= 0 || header.Size > maxAgentArtifactBytes {
			return nil, fmt.Errorf("agent binary has invalid size")
		}
		return io.ReadAll(io.LimitReader(tarReader, maxAgentArtifactBytes+1))
	}
	return nil, fmt.Errorf("agent archive does not contain %s or aks-flex-node", expectedName)
}

func sameContent(path string, expected []byte) bool {
	current, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return false
	}
	currentDigest := sha256.Sum256(current)
	expectedDigest := sha256.Sum256(expected)
	return currentDigest == expectedDigest
}
