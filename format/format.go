// Package format detects compression formats and parses their headers.
package format

import (
	"errors"
	"io"
)

// Format identifies a compression container.
type Format string

const (
	None Format = "none"
	Gzip Format = "gzip"
	Zstd Format = "zstd"
)

// ErrBadHeader reports a header that cannot be parsed.
var ErrBadHeader = errors.New("format: bad header")

// Detect reads the magic bytes of r and seeks it back to the start.
// An input shorter than any magic sequence is None.
func Detect(r io.ReadSeeker) (Format, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return None, err
	}
	var buf [4]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return None, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return None, err
	}
	return DetectBytes(buf[:n]), nil
}

// DetectBytes identifies the format from the leading bytes of a file. A
// zstd skippable frame magic (0x184D2A50..5F) also reports Zstd, so that
// the frame parser can reject it explicitly.
func DetectBytes(b []byte) Format {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		return Gzip
	}
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		return Zstd
	}
	if len(b) >= 4 && b[0]&0xf0 == 0x50 && b[1] == 0x2a && b[2] == 0x4d && b[3] == 0x18 {
		return Zstd
	}
	return None
}
