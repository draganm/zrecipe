package format

import (
	"bufio"
	"bytes"
	"errors"
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

func TestZstdFrameLengthTruncated(t *testing.T) {
	a := zstdAll(t, sample(100000), zstd.WithEncoderCRC(true))
	_, _, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(a[:len(a)-10])))
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("got %v", err)
	}
}
