package format

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrSkippableFrame reports a zstd skippable frame at the start of the input.
var ErrSkippableFrame = errors.New("format: skippable frame")

// ZstdFrameHeader is a parsed zstd frame header (RFC 8878 section 3.1.1).
type ZstdFrameHeader struct {
	HeaderLen      int    // bytes from the magic through the last header field
	WindowLog      int    // 0 when the frame has no window descriptor
	WindowSize     uint64 // from the descriptor, or the content size for single segment frames
	SingleSegment  bool
	Checksum       bool
	HasContentSize bool
	ContentSize    uint64
	DictID         uint32

	// EmptyLastBlock is set by ZstdFrameLength when the frame ends with an
	// empty block after at least one other block. libzstd emits one when
	// ZSTD_e_end is signalled with no input after all data was fed with
	// ZSTD_e_continue.
	EmptyLastBlock bool
	// FirstBlock is the header of the frame's first block, set by
	// ZstdFrameLength.
	FirstBlock ZstdBlockHeader
}

// ZstdBlockType is the Block_Type field of a block header.
type ZstdBlockType int

const (
	ZstdBlockRaw        ZstdBlockType = 0
	ZstdBlockRLE        ZstdBlockType = 1
	ZstdBlockCompressed ZstdBlockType = 2
)

// ZstdBlockHeader is a parsed block header (RFC 8878 section 3.1.1.2).
type ZstdBlockHeader struct {
	Last bool
	Type ZstdBlockType
	// Size is the header's Block_Size field: the content size of a raw or
	// RLE block, the on-disk size of a compressed one.
	Size int
}

// FlushedHead reports the content size of a first block that the producer
// flushed before it was full: a raw or RLE block that is not the last one
// and holds fewer than blockSize bytes (or fewer than the window, when the
// window is smaller). blockSize is the block the producer under
// consideration cuts its input into: 128 KiB for libzstd and klauspost,
// 64 KiB for klauspost's fastest level. A streaming encoder emits only
// full blocks until the end of its input, so a short one at the start
// means the producer wrote that much, flushed, and went on:
// containers/image (skopeo, podman, buildah) writes the bytes it peeked
// at to detect the source compression, then streams the rest through
// klauspost's Encoder.ReadFrom, which flushes what Write buffered first.
func (h *ZstdFrameHeader) FlushedHead(blockSize int) (int, bool) {
	b := h.FirstBlock
	if b.Last || b.Type == ZstdBlockCompressed || b.Size == 0 {
		return 0, false
	}
	full := uint64(blockSize)
	if h.WindowSize > 0 && h.WindowSize < full {
		full = h.WindowSize
	}
	if uint64(b.Size) >= full {
		return 0, false
	}
	return b.Size, true
}

// ParseZstdFrameHeader reads a zstd frame header from r and leaves r at the
// first block header.
func ParseZstdFrameHeader(r *bufio.Reader) (*ZstdFrameHeader, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("%w: zstd magic: %v", ErrBadHeader, err)
	}
	if magic[0]&0xf0 == 0x50 && magic[1] == 0x2a && magic[2] == 0x4d && magic[3] == 0x18 {
		return nil, ErrSkippableFrame
	}
	if magic != [4]byte{0x28, 0xb5, 0x2f, 0xfd} {
		return nil, fmt.Errorf("%w: not a zstd magic", ErrBadHeader)
	}
	fhd, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("%w: zstd frame header descriptor: %v", ErrBadHeader, err)
	}
	if fhd&0x08 != 0 {
		return nil, fmt.Errorf("%w: zstd reserved descriptor bit set", ErrBadHeader)
	}
	h := &ZstdFrameHeader{
		HeaderLen:     5,
		SingleSegment: fhd&0x20 != 0,
		Checksum:      fhd&0x04 != 0,
	}
	if !h.SingleSegment {
		wd, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: zstd window descriptor: %v", ErrBadHeader, err)
		}
		h.HeaderLen++
		h.WindowLog = 10 + int(wd>>3)
		base := uint64(1) << h.WindowLog
		h.WindowSize = base + (base/8)*uint64(wd&7)
	}
	dictSize := [4]int{0, 1, 2, 4}[fhd&0x03]
	if dictSize > 0 {
		var b [4]byte
		if _, err := io.ReadFull(r, b[:dictSize]); err != nil {
			return nil, fmt.Errorf("%w: zstd dictionary id: %v", ErrBadHeader, err)
		}
		h.HeaderLen += dictSize
		h.DictID = binary.LittleEndian.Uint32(b[:])
	}
	fcsSize := 0
	switch fhd >> 6 {
	case 0:
		if h.SingleSegment {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	if fcsSize > 0 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:fcsSize]); err != nil {
			return nil, fmt.Errorf("%w: zstd content size: %v", ErrBadHeader, err)
		}
		h.HeaderLen += fcsSize
		h.HasContentSize = true
		h.ContentSize = binary.LittleEndian.Uint64(b[:])
		if fcsSize == 2 {
			h.ContentSize += 256
		}
	}
	if h.SingleSegment {
		h.WindowSize = h.ContentSize
	}
	return h, nil
}

// ZstdFrameLength parses the frame header and walks the block headers,
// without decompressing, to find the total length of the first frame in r
// including its checksum. r is consumed up to the end of the frame.
func ZstdFrameLength(r *bufio.Reader) (*ZstdFrameHeader, int64, error) {
	h, err := ParseZstdFrameHeader(r)
	if err != nil {
		return nil, 0, err
	}
	n := int64(h.HeaderLen)
	blocks := 0
	for {
		var bh [3]byte
		if _, err := io.ReadFull(r, bh[:]); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd block header: %v", ErrBadHeader, err)
		}
		v := uint32(bh[0]) | uint32(bh[1])<<8 | uint32(bh[2])<<16
		last := v&1 != 0
		typ := (v >> 1) & 3
		size := int(v >> 3)
		if typ == 3 {
			return nil, 0, fmt.Errorf("%w: zstd reserved block type", ErrBadHeader)
		}
		if blocks == 0 {
			h.FirstBlock = ZstdBlockHeader{Last: last, Type: ZstdBlockType(typ), Size: size}
		}
		if typ == 1 { // RLE block: one byte on disk
			size = 1
		}
		if _, err := r.Discard(size); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd block truncated: %v", ErrBadHeader, err)
		}
		n += 3 + int64(size)
		blocks++
		if last {
			if typ == 0 && v>>3 == 0 && blocks > 1 {
				h.EmptyLastBlock = true
			}
			break
		}
	}
	if h.Checksum {
		if _, err := r.Discard(4); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd checksum truncated: %v", ErrBadHeader, err)
		}
		n += 4
	}
	return h, n, nil
}
