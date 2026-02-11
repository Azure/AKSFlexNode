package units

import (
	"archive/tar"
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// extractDeb extracts the file contents of a Debian package at src into base.
//
// Debian package format:
//
//	┌──────────────────────────┐
//	│ ar archive               │
//	│  ├── debian-binary       │  ← version string (e.g. "2.0\n")
//	│  ├── control.tar.gz      │  ← package metadata (skipped)
//	│  └── data.tar.gz         │  ← actual file contents to extract
//	└──────────────────────────┘
//
// The ar archive uses the common Unix "ar" format. Each member has a 60-byte
// header followed by the member data. The data.tar member may use gzip (.gz),
// xz (.xz), zstd (.zst), or no compression. The compression format is
// detected automatically from magic bytes.
func extractDeb(src, base string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening deb file %s: %w", src, err)
	}
	defer f.Close()

	// Verify and skip the ar global header ("!<arch>\n", 8 bytes).
	var arMagic [8]byte
	if _, err := io.ReadFull(f, arMagic[:]); err != nil {
		return fmt.Errorf("reading ar magic: %w", err)
	}
	if string(arMagic[:]) != "!<arch>\n" {
		return fmt.Errorf("invalid ar magic: %q", arMagic)
	}

	// Iterate through ar members looking for data.tar*.
	for {
		name, size, err := readArHeader(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading ar member header: %w", err)
		}

		memberReader := io.LimitReader(f, size)

		if strings.HasPrefix(name, "data.tar") {
			return extractDebData(name, memberReader, base)
		}

		// Skip this member's data.
		if _, err := io.Copy(io.Discard, memberReader); err != nil {
			return fmt.Errorf("skipping ar member %q: %w", name, err)
		}

		// ar members are padded to 2-byte boundaries.
		if size%2 != 0 {
			if _, err := io.ReadFull(f, make([]byte, 1)); err != nil {
				return fmt.Errorf("reading ar padding: %w", err)
			}
		}
	}

	return fmt.Errorf("data.tar member not found in deb archive %s", src)
}

// arHeaderSize is the size of a Unix ar member header.
const arHeaderSize = 60

// readArHeader reads a single ar member header and returns the member name
// and data size. The caller is responsible for reading exactly `size` bytes
// of member data before calling readArHeader again.
func readArHeader(r io.Reader) (name string, size int64, err error) {
	var hdr [arHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return "", 0, io.EOF
		}
		return "", 0, err
	}

	// ar header layout:
	//   0-15:  name (16 bytes, space-padded, terminated with '/')
	//   16-27: modification time (12 bytes)
	//   28-33: owner ID (6 bytes)
	//   34-39: group ID (6 bytes)
	//   40-47: file mode (8 bytes)
	//   48-57: file size in bytes (10 bytes, decimal, space-padded)
	//   58-59: magic "`\n" (2 bytes)
	name = strings.TrimRight(string(hdr[0:16]), " ")
	name = strings.TrimSuffix(name, "/")

	sizeStr := strings.TrimSpace(string(hdr[48:58]))
	size, err = strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("parsing ar member size %q: %w", sizeStr, err)
	}

	return name, size, nil
}

// extractDebData extracts the contents of a data.tar* member into base.
// Compression is detected automatically from magic bytes.
func extractDebData(name string, r io.Reader, base string) error {
	var tr *tar.Reader

	if name == "data.tar" {
		// Uncompressed data.tar.
		tr = tar.NewReader(r)
	} else {
		// Compressed — detect format from magic bytes.
		br := bufio.NewReader(r)
		decompressed, err := newDecompressor(br)
		if err != nil {
			return fmt.Errorf("decompressing %s: %w", name, err)
		}
		if closer, ok := decompressed.(io.Closer); ok {
			defer closer.Close()
		}
		tr = tar.NewReader(decompressed)
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry from %s: %w", name, err)
		}

		// Strip leading "./" from paths (common in deb data.tar).
		entryName := hdr.Name
		if strings.HasPrefix(entryName, "./") {
			entryName = entryName[2:]
		}
		if entryName == "" || entryName == "." {
			continue
		}

		target, err := safePath(base, entryName)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := extractEntry(target, 0, true, nil); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := extractEntry(target, os.FileMode(hdr.Mode), false, tr); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := extractSymlink(base, target, hdr.Linkname); err != nil {
				return err
			}
		}
	}

	return nil
}
