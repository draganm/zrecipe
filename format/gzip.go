package format

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// GzipHeader is a parsed gzip member header. Raw holds the exact header
// bytes, from byte 0 up to the first byte of the deflate stream.
type GzipHeader struct {
	Raw     []byte
	Flags   byte
	ModTime uint32
	XFL     byte
	OS      byte
}

const (
	gzipFHCRC    = 0x02
	gzipFEXTRA   = 0x04
	gzipFNAME    = 0x08
	gzipFCOMMENT = 0x10
)

// ParseGzipHeader reads a gzip member header from r and leaves r positioned
// at the first byte of the deflate stream.
func ParseGzipHeader(r *bufio.Reader) (*GzipHeader, error) {
	fixed := make([]byte, 10)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, fmt.Errorf("%w: gzip fixed header: %v", ErrBadHeader, err)
	}
	if fixed[0] != 0x1f || fixed[1] != 0x8b {
		return nil, fmt.Errorf("%w: not a gzip magic", ErrBadHeader)
	}
	if fixed[2] != 8 {
		return nil, fmt.Errorf("%w: gzip compression method %d", ErrBadHeader, fixed[2])
	}
	h := &GzipHeader{
		Raw:     append([]byte{}, fixed...),
		Flags:   fixed[3],
		ModTime: binary.LittleEndian.Uint32(fixed[4:8]),
		XFL:     fixed[8],
		OS:      fixed[9],
	}
	if h.Flags&gzipFEXTRA != 0 {
		var xlen [2]byte
		if _, err := io.ReadFull(r, xlen[:]); err != nil {
			return nil, fmt.Errorf("%w: gzip extra length: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, xlen[:]...)
		extra := make([]byte, binary.LittleEndian.Uint16(xlen[:]))
		if _, err := io.ReadFull(r, extra); err != nil {
			return nil, fmt.Errorf("%w: gzip extra field: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, extra...)
	}
	for _, f := range []struct {
		flag byte
		name string
	}{{gzipFNAME, "name"}, {gzipFCOMMENT, "comment"}} {
		if h.Flags&f.flag == 0 {
			continue
		}
		s, err := r.ReadBytes(0)
		if err != nil {
			return nil, fmt.Errorf("%w: gzip %s: %v", ErrBadHeader, f.name, err)
		}
		h.Raw = append(h.Raw, s...)
	}
	if h.Flags&gzipFHCRC != 0 {
		var crc [2]byte
		if _, err := io.ReadFull(r, crc[:]); err != nil {
			return nil, fmt.Errorf("%w: gzip header crc: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, crc[:]...)
	}
	return h, nil
}
