package format

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdAll(t *testing.T, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, append(opts, zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true))...)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

func zstdStream(t *testing.T, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, append(opts, zstd.WithEncoderConcurrency(1))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sample(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/13)
	}
	return b
}

func parse(t *testing.T, frame []byte) *ZstdFrameHeader {
	t.Helper()
	h, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestZstdHeaderSingleSegmentWithChecksum(t *testing.T) {
	data := sample(1000)
	h := parse(t, zstdAll(t, data, zstd.WithEncoderCRC(true), zstd.WithSingleSegment(true)))
	if !h.SingleSegment || !h.Checksum || !h.HasContentSize || h.ContentSize != 1000 {
		t.Fatalf("%+v", h)
	}
	if h.WindowLog != 0 || h.WindowSize != 1000 {
		t.Fatalf("window: %+v", h)
	}
}

func TestZstdHeaderStreamed(t *testing.T) {
	h := parse(t, zstdStream(t, sample(300000), zstd.WithEncoderCRC(false), zstd.WithWindowSize(1<<16)))
	if h.SingleSegment || h.Checksum || h.HasContentSize {
		t.Fatalf("%+v", h)
	}
	if h.WindowLog != 16 || h.WindowSize != 1<<16 {
		t.Fatalf("window: %+v", h)
	}
}

func TestZstdHeaderDictID(t *testing.T) {
	// Magic, descriptor with dict id flag 2 (2 bytes), window descriptor, dict id 0x1234.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x02, 0x40, 0x34, 0x12}
	h := parse(t, frame)
	if h.DictID != 0x1234 || h.HeaderLen != 8 {
		t.Fatalf("%+v", h)
	}
}

func TestZstdHeaderSkippable(t *testing.T) {
	frame := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	_, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if !errors.Is(err, ErrSkippableFrame) {
		t.Fatalf("got %v", err)
	}
}

func TestZstdHeaderErrors(t *testing.T) {
	for name, in := range map[string][]byte{
		"truncated": {0x28, 0xb5},
		"bad magic": {0x28, 0xb5, 0x2f, 0xfe, 0},
		"reserved":  {0x28, 0xb5, 0x2f, 0xfd, 0x08, 0x40},
	} {
		_, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(in)))
		if !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestZstdFrameLength(t *testing.T) {
	for name, frame := range map[string][]byte{
		"all crc":      zstdAll(t, sample(500000), zstd.WithEncoderCRC(true)),
		"all nocrc":    zstdAll(t, sample(500000), zstd.WithEncoderCRC(false)),
		"stream crc":   zstdStream(t, sample(500000), zstd.WithEncoderCRC(true)),
		"empty":        zstdAll(t, nil, zstd.WithEncoderCRC(true)),
		"zeros":        zstdStream(t, make([]byte, 1<<20)),
		"single block": zstdAll(t, []byte("tiny"), zstd.WithEncoderCRC(false)),
	} {
		_, n, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(frame)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if n != int64(len(frame)) {
			t.Errorf("%s: length %d, want %d", name, n, len(frame))
		}
	}
}

func TestZstdFrameLengthStopsAtFirstFrame(t *testing.T) {
	a := zstdAll(t, sample(1000), zstd.WithEncoderCRC(true))
	b := zstdAll(t, sample(2000), zstd.WithEncoderCRC(true))
	_, n, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(append(append([]byte{}, a...), b...))))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(a)) {
		t.Fatalf("length %d, want %d", n, len(a))
	}
}

func TestZstdFrameLengthEmptyLastBlock(t *testing.T) {
	orig := zstdStream(t, sample(300000))

	h, n, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(orig)))
	if err != nil {
		t.Fatal(err)
	}
	if h.EmptyLastBlock {
		t.Fatal("original frame reported EmptyLastBlock")
	}
	if n != int64(len(orig)) {
		t.Fatalf("length %d, want %d", n, len(orig))
	}

	// Walk the blocks by hand to find the header of the block currently
	// carrying the last-block flag.
	r := bufio.NewReader(bytes.NewReader(orig))
	hdr, err := ParseZstdFrameHeader(r)
	if err != nil {
		t.Fatal(err)
	}
	pos := hdr.HeaderLen
	lastHeaderPos := -1
	for {
		var bh [3]byte
		if _, err := io.ReadFull(r, bh[:]); err != nil {
			t.Fatal(err)
		}
		v := uint32(bh[0]) | uint32(bh[1])<<8 | uint32(bh[2])<<16
		last := v&1 != 0
		size := int(v >> 3)
		if (v>>1)&3 == 1 { // RLE block: one byte on disk
			size = 1
		}
		lastHeaderPos = pos
		pos += 3 + size
		if _, err := r.Discard(size); err != nil {
			t.Fatal(err)
		}
		if last {
			break
		}
	}

	// Clear the last-flag bit on that block, then splice in an empty raw
	// last block (0x01, 0x00, 0x00: last=1, type=raw, size=0) right after
	// it, before the checksum (if any).
	modified := append([]byte{}, orig[:lastHeaderPos]...)
	modified = append(modified, orig[lastHeaderPos]&^0x01)
	modified = append(modified, orig[lastHeaderPos+1:pos]...)
	modified = append(modified, 0x01, 0x00, 0x00)
	modified = append(modified, orig[pos:]...)

	h2, n2, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(modified)))
	if err != nil {
		t.Fatal(err)
	}
	if !h2.EmptyLastBlock {
		t.Fatal("modified frame did not report EmptyLastBlock")
	}
	if n2 != int64(len(modified)) {
		t.Fatalf("length %d, want %d", n2, len(modified))
	}

	h3, _, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdAll(t, nil, zstd.WithEncoderCRC(true)))))
	if err != nil {
		t.Fatal(err)
	}
	if h3.EmptyLastBlock {
		t.Fatal("single-block empty-input frame reported EmptyLastBlock")
	}
}

func TestZstdFrameLengthTruncated(t *testing.T) {
	a := zstdAll(t, sample(100000), zstd.WithEncoderCRC(true))
	_, _, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(a[:len(a)-10])))
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("got %v", err)
	}
}

// zstdHeadFlush is the shape containers/image produces: the first head
// bytes go through Write, then the encoder is flushed and the rest is
// streamed, which is what Encoder.ReadFrom does after a Write.
func zstdHeadFlush(t *testing.T, data []byte, head int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data[:head]); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data[head:]); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestZstdFrameLengthFirstBlock(t *testing.T) {
	h, _, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdHeadFlush(t, sample(300000), 8))))
	if err != nil {
		t.Fatal(err)
	}
	if h.FirstBlock.Last || h.FirstBlock.Type != ZstdBlockRaw || h.FirstBlock.Size != 8 {
		t.Fatalf("first block %+v, want a raw non-last block of 8 bytes", h.FirstBlock)
	}
	if n, ok := h.FlushedHead(128 << 10); !ok || n != 8 {
		t.Fatalf("FlushedHead = %d, %v; want 8, true", n, ok)
	}

	h, _, err = ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdStream(t, sample(300000)))))
	if err != nil {
		t.Fatal(err)
	}
	if h.FirstBlock.Last || h.FirstBlock.Type != ZstdBlockCompressed {
		t.Fatalf("first block %+v, want a compressed non-last block", h.FirstBlock)
	}
	if _, ok := h.FlushedHead(128 << 10); ok {
		t.Fatal("a plain stream reported a flushed head")
	}

	// A small input flushed after its head: the head block is still not
	// the last one and still counts.
	h, _, err = ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdHeadFlush(t, sample(1000), 8))))
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := h.FlushedHead(128 << 10); !ok || n != 8 {
		t.Fatalf("FlushedHead = %d, %v; want 8, true", n, ok)
	}

	// A tiny frame is one block, raw and last: nothing was flushed early.
	h, _, err = ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdAll(t, []byte("tiny"), zstd.WithEncoderCRC(false)))))
	if err != nil {
		t.Fatal(err)
	}
	if !h.FirstBlock.Last || h.FirstBlock.Type != ZstdBlockRaw || h.FirstBlock.Size != 4 {
		t.Fatalf("first block %+v, want a raw last block of 4 bytes", h.FirstBlock)
	}
	if _, ok := h.FlushedHead(128 << 10); ok {
		t.Fatal("a single-block frame reported a flushed head")
	}

	// A full-size raw first block is what any encoder emits for
	// incompressible input: not a flush.
	h, _, err = ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdStream(t, random(300000)))))
	if err != nil {
		t.Fatal(err)
	}
	if h.FirstBlock.Type != ZstdBlockRaw || h.FirstBlock.Size != 128<<10 {
		t.Fatalf("first block %+v, want a raw block of 128 KiB", h.FirstBlock)
	}
	if _, ok := h.FlushedHead(128 << 10); ok {
		t.Fatal("a full raw first block reported a flushed head")
	}

	// klauspost's fastest level cuts 64 KiB blocks: a raw or RLE block of
	// that size is full for it and short for a 128 KiB producer.
	h, _, err = ZstdFrameLength(bufio.NewReader(bytes.NewReader(zstdStream(t, make([]byte, 300000), zstd.WithEncoderLevel(zstd.SpeedFastest)))))
	if err != nil {
		t.Fatal(err)
	}
	if h.FirstBlock.Type != ZstdBlockRLE || h.FirstBlock.Size != 64<<10 {
		t.Fatalf("first block %+v, want an RLE block of 64 KiB", h.FirstBlock)
	}
	if _, ok := h.FlushedHead(64 << 10); ok {
		t.Fatal("a full 64 KiB block reported a flushed head for a 64 KiB producer")
	}
	if n, ok := h.FlushedHead(128 << 10); !ok || n != 64<<10 {
		t.Fatalf("FlushedHead(128 KiB) = %d, %v; want 65536, true", n, ok)
	}

	// ParseZstdFrameHeader stops before the blocks: the zero FirstBlock it
	// leaves is not a flushed head either.
	if _, ok := parse(t, zstdHeadFlush(t, sample(300000), 8)).FlushedHead(128 << 10); ok {
		t.Fatal("an unparsed first block reported a flushed head")
	}
}

func random(n int) []byte {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}
