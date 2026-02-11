package units

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// extractRPM extracts the file contents of an RPM package at src into base.
//
// RPM file format (simplified):
//
//	┌────────────────────┐
//	│ Lead (96 bytes)    │
//	├────────────────────┤
//	│ Signature header   │  ← RPM header structure
//	├────────────────────┤
//	│ Main header        │  ← RPM header structure
//	├────────────────────┤
//	│ Payload            │  ← compressed CPIO archive
//	└────────────────────┘
//
// Each RPM header starts with the magic bytes [0x8e, 0xad, 0xe8, 0x01],
// followed by 4 reserved bytes, then a big-endian uint32 index count
// and a big-endian uint32 data size. The header region spans
// 16 + (indexCount * 16) + dataSize bytes (from the magic onward).
// The signature header is additionally padded to an 8-byte boundary.
//
// The payload is a compressed CPIO archive in the "new ASCII" (newc)
// format. The compression is detected by magic bytes and may be
// gzip, zstd, or xz.
func extractRPM(src, base string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening rpm file %s: %w", src, err)
	}
	defer f.Close()

	// Skip the 96-byte RPM lead.
	if _, err := f.Seek(96, io.SeekStart); err != nil {
		return fmt.Errorf("seeking past rpm lead: %w", err)
	}

	// Skip the signature header (with 8-byte alignment padding).
	if err := skipRPMHeader(f, true); err != nil {
		return fmt.Errorf("skipping rpm signature header: %w", err)
	}

	// Skip the main header (no alignment padding).
	if err := skipRPMHeader(f, false); err != nil {
		return fmt.Errorf("skipping rpm main header: %w", err)
	}

	// Detect compression from payload magic bytes using a buffered reader
	// so we can peek without consuming bytes.
	br := bufio.NewReader(f)
	decompressed, err := newDecompressor(br)
	if err != nil {
		return fmt.Errorf("decompressing rpm payload from %s: %w", src, err)
	}
	if closer, ok := decompressed.(io.Closer); ok {
		defer closer.Close()
	}

	return extractCPIO(decompressed, base)
}

// rpmHeaderMagic is the magic number that begins every RPM header structure.
var rpmHeaderMagic = [4]byte{0x8e, 0xad, 0xe8, 0x01}

// skipRPMHeader reads and skips an RPM header structure. If alignTo8 is true,
// the file position is advanced to the next 8-byte boundary after the header
// data (required for the signature header).
func skipRPMHeader(r io.ReadSeeker, alignTo8 bool) error {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return fmt.Errorf("reading header magic: %w", err)
	}
	if magic != rpmHeaderMagic {
		return fmt.Errorf("invalid rpm header magic: %x", magic)
	}

	// Skip 4 reserved bytes.
	if _, err := r.Seek(4, io.SeekCurrent); err != nil {
		return fmt.Errorf("seeking past reserved bytes: %w", err)
	}

	// Read index count and data size (both big-endian uint32).
	var indexCount, dataSize uint32
	if err := binary.Read(r, binary.BigEndian, &indexCount); err != nil {
		return fmt.Errorf("reading index count: %w", err)
	}
	if err := binary.Read(r, binary.BigEndian, &dataSize); err != nil {
		return fmt.Errorf("reading data size: %w", err)
	}

	// Each index entry is 16 bytes.
	skip := int64(indexCount)*16 + int64(dataSize)
	if _, err := r.Seek(skip, io.SeekCurrent); err != nil {
		return fmt.Errorf("seeking past header data: %w", err)
	}

	if alignTo8 {
		// The total header region size (from the start of magic) is:
		// 16 (magic + reserved + counts) + indexCount*16 + dataSize
		headerSize := 16 + int64(indexCount)*16 + int64(dataSize)
		pad := (8 - (headerSize % 8)) % 8
		if pad > 0 {
			if _, err := r.Seek(pad, io.SeekCurrent); err != nil {
				return fmt.Errorf("seeking past alignment padding: %w", err)
			}
		}
	}

	return nil
}

// cpioNewcHeaderSize is the fixed size of a CPIO "new ASCII" (newc) header.
const cpioNewcHeaderSize = 110

// cpioNewcMagic is the magic string for the CPIO "new ASCII" format.
const cpioNewcMagic = "070701"

// cpioTrailer is the filename that marks the end of a CPIO archive.
const cpioTrailer = "TRAILER!!!"

// extractCPIO extracts entries from a CPIO "new ASCII" (newc) stream into base.
func extractCPIO(r io.Reader, base string) error {
	for {
		// Read the fixed-size header.
		var hdr [cpioNewcHeaderSize]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return fmt.Errorf("reading cpio header: %w", err)
		}

		magic := string(hdr[0:6])
		if magic != cpioNewcMagic {
			return fmt.Errorf("invalid cpio magic: %q", magic)
		}

		mode := cpioParseHex([8]byte(hdr[14:22]))
		fileSize := cpioParseHex([8]byte(hdr[54:62]))
		nameSize := cpioParseHex([8]byte(hdr[94:102]))

		// Read the filename.
		nameBuf := make([]byte, nameSize)
		if _, err := io.ReadFull(r, nameBuf); err != nil {
			return fmt.Errorf("reading cpio filename: %w", err)
		}
		// Trim the null terminator.
		name := string(nameBuf[:nameSize-1])

		// Skip padding after header + name to align to 4 bytes.
		headerAndName := cpioNewcHeaderSize + int64(nameSize)
		namePad := (4 - (headerAndName % 4)) % 4
		if namePad > 0 {
			if _, err := io.ReadFull(r, make([]byte, namePad)); err != nil {
				return fmt.Errorf("reading cpio name padding: %w", err)
			}
		}

		// Check for the archive trailer.
		if name == cpioTrailer {
			return nil
		}

		// Read the file data.
		dataReader := io.LimitReader(r, int64(fileSize))

		if name == "." || name == "./" {
			// Skip root directory entries.
			if _, err := io.Copy(io.Discard, dataReader); err != nil {
				return fmt.Errorf("discarding cpio root entry: %w", err)
			}
		} else {
			// Strip leading "./" or "/" from the name for safe extraction.
			cleanName := name
			for len(cleanName) > 0 && cleanName[0] == '/' {
				cleanName = cleanName[1:]
			}
			if len(cleanName) >= 2 && cleanName[:2] == "./" {
				cleanName = cleanName[2:]
			}

			if cleanName != "" {
				target, err := safePath(base, cleanName)
				if err != nil {
					return err
				}

				isDir := (mode & 0o40000) != 0
				fileMode := os.FileMode(mode & 0o7777)

				if isDir {
					if err := extractEntry(target, fileMode, true, nil); err != nil {
						return err
					}
					if _, err := io.Copy(io.Discard, dataReader); err != nil {
						return fmt.Errorf("discarding cpio dir data: %w", err)
					}
				} else if (mode & 0o120000) == 0o120000 {
					// Symlink — the data is the link target.
					linkBuf, err := io.ReadAll(dataReader)
					if err != nil {
						return fmt.Errorf("reading cpio symlink target for %q: %w", name, err)
					}
					if err := extractSymlink(base, target, string(linkBuf)); err != nil {
						return err
					}
				} else if (mode & 0o100000) != 0 {
					// Regular file.
					if err := extractEntry(target, fileMode, false, dataReader); err != nil {
						return err
					}
				} else {
					// Skip other entry types (symlinks, devices, etc.).
					if _, err := io.Copy(io.Discard, dataReader); err != nil {
						return fmt.Errorf("discarding cpio entry %q: %w", name, err)
					}
				}
			} else {
				if _, err := io.Copy(io.Discard, dataReader); err != nil {
					return fmt.Errorf("discarding cpio entry: %w", err)
				}
			}
		}

		// Skip padding after data to align to 4 bytes.
		dataPad := (4 - (int64(fileSize) % 4)) % 4
		if dataPad > 0 {
			if _, err := io.ReadFull(r, make([]byte, dataPad)); err != nil {
				return fmt.Errorf("reading cpio data padding: %w", err)
			}
		}
	}
}

// cpioParseHex parses an 8-byte ASCII hex field from a CPIO header.
func cpioParseHex(b [8]byte) int64 {
	var n int64
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int64(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int64(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			n |= int64(c - 'A' + 10)
		}
	}
	return n
}
