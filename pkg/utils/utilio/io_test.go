package utilio

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFile(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string // returns the target filename
		content []byte
		perm    os.FileMode
		wantErr bool
	}{
		{
			name: "writes file to existing directory",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "test.txt")
			},
			content: []byte("hello"),
			perm:    0644,
		},
		{
			name: "creates parent directories as needed",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "a", "b", "c", "test.txt")
			},
			content: []byte("nested"),
			perm:    0644,
		},
		{
			name: "overwrites existing file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				fname := filepath.Join(dir, "existing.txt")
				if err := os.WriteFile(fname, []byte("old"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return fname
			},
			content: []byte("new content"),
			perm:    0644,
		},
		{
			name: "writes empty content",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "empty.txt")
			},
			content: []byte{},
			perm:    0644,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filename := tt.setup(t)
			err := WriteFile(filename, tt.content, tt.perm)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := os.ReadFile(filename)
			if err != nil {
				t.Fatalf("failed to read written file: %v", err)
			}
			if !bytes.Equal(got, tt.content) {
				t.Fatalf("file content mismatch: got %q, want %q", got, tt.content)
			}
		})
	}
}

func TestInstallFile(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		input   io.Reader
		perm    os.FileMode
		wantErr bool
		errIs   error
		want    []byte
	}{
		{
			name: "installs file from reader",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "installed.txt")
			},
			input: strings.NewReader("file content"),
			perm:  0644,
			want:  []byte("file content"),
		},
		{
			name: "creates nested directories",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "deep", "nested", "file.bin")
			},
			input: bytes.NewReader([]byte{0x00, 0x01, 0x02}),
			perm:  0755,
			want:  []byte{0x00, 0x01, 0x02},
		},
		{
			name: "reader error is propagated",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "fail.txt")
			},
			input:   &errReader{err: errors.New("broken reader")},
			perm:    0644,
			wantErr: true,
		},
		{
			name: "file exceeding 1 GiB is rejected",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "big.txt")
			},
			input:   io.LimitReader(&zeroReader{}, 1*1024*1024*1024+1),
			perm:    0644,
			wantErr: true,
			errIs:   ErrFileTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filename := tt.setup(t)
			err := InstallFile(filename, tt.input, tt.perm)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errIs != nil && !errors.Is(err, tt.errIs) {
					t.Fatalf("expected error wrapping %v, got %v", tt.errIs, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := os.ReadFile(filename)
			if err != nil {
				t.Fatalf("failed to read installed file: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("file content mismatch: got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInstallFileWithLimitedSize(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T) string
		input     io.Reader
		perm      os.FileMode
		maxBytes  int64
		wantErr   bool
		errIs     error
		errSubstr string
		want      []byte
	}{
		{
			name: "writes small content within limit",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "small.txt")
			},
			input:    strings.NewReader("hello world"),
			perm:     0644,
			maxBytes: 1024,
			want:     []byte("hello world"),
		},
		{
			name: "writes empty content",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "empty.txt")
			},
			input:    strings.NewReader(""),
			perm:     0644,
			maxBytes: 100,
			want:     []byte{},
		},
		{
			name: "content exactly at limit is accepted",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "exact.txt")
			},
			input:    strings.NewReader("12345"),
			perm:     0644,
			maxBytes: 5,
			want:     []byte("12345"),
		},
		{
			name: "content exceeding limit by one byte returns ErrFileTooLarge",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "over.txt")
			},
			input:    strings.NewReader("123456"),
			perm:     0644,
			maxBytes: 5,
			wantErr:  true,
			errIs:    ErrFileTooLarge,
		},
		{
			name: "content far exceeding limit returns ErrFileTooLarge",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "big.txt")
			},
			input:    io.LimitReader(&zeroReader{}, 10*1024*1024),
			perm:     0644,
			maxBytes: 1024,
			wantErr:  true,
			errIs:    ErrFileTooLarge,
		},
		{
			name: "error message includes byte counts",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "msg.txt")
			},
			input:     strings.NewReader("abcdef"),
			perm:      0644,
			maxBytes:  5,
			wantErr:   true,
			errIs:     ErrFileTooLarge,
			errSubstr: "6 bytes exceeds limit 5",
		},
		{
			name: "zero maxBytes returns error",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "zero.txt")
			},
			input:     strings.NewReader("data"),
			perm:      0644,
			maxBytes:  0,
			wantErr:   true,
			errSubstr: "invalid maxBytes",
		},
		{
			name: "negative maxBytes returns error",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "neg.txt")
			},
			input:     strings.NewReader("data"),
			perm:      0644,
			maxBytes:  -1,
			wantErr:   true,
			errSubstr: "invalid maxBytes",
		},
		{
			name: "creates nested parent directories",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "a", "b", "c", "file.txt")
			},
			input:    strings.NewReader("nested content"),
			perm:     0644,
			maxBytes: 1024,
			want:     []byte("nested content"),
		},
		{
			name: "reader error is propagated",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "err.txt")
			},
			input:    &errReader{err: errors.New("read failure")},
			perm:     0644,
			maxBytes: 1024,
			wantErr:  true,
		},
		{
			name: "binary content is preserved",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "binary.bin")
			},
			input:    bytes.NewReader([]byte{0x00, 0xFF, 0x80, 0x01}),
			perm:     0755,
			maxBytes: 100,
			want:     []byte{0x00, 0xFF, 0x80, 0x01},
		},
		{
			name: "file not created on size limit exceeded",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "not-created.txt")
			},
			input:    strings.NewReader("too much data"),
			perm:     0644,
			maxBytes: 5,
			wantErr:  true,
			errIs:    ErrFileTooLarge,
		},
		{
			name: "overwrites existing file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				fname := filepath.Join(dir, "existing.txt")
				if err := os.WriteFile(fname, []byte("old data"), 0644); err != nil {
					t.Fatalf("setup: %v", err)
				}
				return fname
			},
			input:    strings.NewReader("new data"),
			perm:     0644,
			maxBytes: 1024,
			want:     []byte("new data"),
		},
		{
			name: "maxBytes of 1 accepts single byte",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "one.txt")
			},
			input:    strings.NewReader("x"),
			perm:     0644,
			maxBytes: 1,
			want:     []byte("x"),
		},
		{
			name: "maxBytes of 1 rejects two bytes",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "two.txt")
			},
			input:    strings.NewReader("xy"),
			perm:     0644,
			maxBytes: 1,
			wantErr:  true,
			errIs:    ErrFileTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			filename := tt.setup(t)
			err := InstallFileWithLimitedSize(filename, tt.input, tt.perm, tt.maxBytes)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tt.errIs != nil && !errors.Is(err, tt.errIs) {
					t.Fatalf("expected error wrapping %v, got %v", tt.errIs, err)
				}
				if tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("expected error containing %q, got %q", tt.errSubstr, err.Error())
				}
				// Verify file is not left behind on error when ErrFileTooLarge
				if tt.errIs == ErrFileTooLarge {
					if _, statErr := os.Stat(filename); statErr == nil {
						t.Fatalf("expected file not to exist after size limit error, but it does")
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			got, err := os.ReadFile(filename)
			if err != nil {
				t.Fatalf("failed to read file: %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("file content mismatch: got %q, want %q", got, tt.want)
			}
		})
	}
}

// zeroReader is an infinite reader that produces zero bytes.
type zeroReader struct{}

func (z *zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// errReader always returns the configured error.
type errReader struct {
	err error
}

func (e *errReader) Read(_ []byte) (int, error) {
	return 0, e.err
}
