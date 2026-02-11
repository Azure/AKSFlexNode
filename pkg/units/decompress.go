package units

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// newDecompressor peeks at the first few bytes of a buffered reader to detect
// the compression format and returns the appropriate decompressing reader.
// Supported formats: gzip, zstd, xz. If the stream is uncompressed (or the
// format is not recognized), an error is returned.
func newDecompressor(br *bufio.Reader) (io.Reader, error) {
	// We need at least 6 bytes to distinguish between all formats.
	// xz magic is 6 bytes: FD 37 7A 58 5A 00
	magic, err := br.Peek(6)
	if err != nil {
		return nil, fmt.Errorf("peeking at compression magic: %w", err)
	}

	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b:
		// gzip magic: 1F 8B
		return gzip.NewReader(br)

	case magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		// zstd magic: 28 B5 2F FD
		dec, err := zstd.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("creating zstd reader: %w", err)
		}
		return dec, nil

	case magic[0] == 0xfd && magic[1] == 0x37 && magic[2] == 0x7a &&
		magic[3] == 0x58 && magic[4] == 0x5a && magic[5] == 0x00:
		// xz magic: FD 37 7A 58 5A 00
		return xz.NewReader(br)

	default:
		return nil, fmt.Errorf("unsupported compression format (magic: %x)", magic)
	}
}
