package kpzstd

import (
	"bufio"
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/format"
)

func header(t *testing.T, frame []byte) *format.ZstdFrameHeader {
	t.Helper()
	h, err := format.ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "klauspost-zstd" || e.Format() != engine.FormatZstd {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	// Note: go test binaries omit the dependency list from runtime/debug.ReadBuildInfo,
	// so engine.ModuleVersion returns "(devel)". The actual version v1.20.0 is observable
	// only in a built binary (e.g., the CLI), which the integration tests cover.
	v := e.Version()
	if v != "v1.20.0" && v != "(devel)" {
		t.Fatalf("version %q; update this test when bumping klauspost/compress", v)
	}
}

func TestCandidatesFromStreamedHeader(t *testing.T) {
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
	// Above the 128 KiB default block size, so Write crosses a block boundary
	// and Close takes the genuine multi-block streaming path (no single
	// segment, no content size), instead of klauspost's single-block fast path.
	enc.Write(fixtures.Text(300000))
	enc.Close()
	tiers := New().Candidates(header(t, buf.Bytes()), 300000)
	if len(tiers) != 1 || len(tiers[0]) != 4 {
		t.Fatalf("%+v", tiers)
	}
	if p := tiers[0][0]; p.Level != 2 || p.ContentSize || p.SingleSegment || p.EncodeAll || !p.Checksum {
		t.Fatalf("first %+v", p)
	}
}

func TestCandidatesFromEncodeAllHeader(t *testing.T) {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	// Above klauspost's 1024-byte MinWindowSize, so EncodeAll's single-segment
	// heuristic (len(src) > MinWindowSize) sets the single segment flag.
	frame := enc.EncodeAll(fixtures.Text(5000), nil)
	tiers := New().Candidates(header(t, frame), 5000)
	if p := tiers[0][0]; !p.SingleSegment || !p.ContentSize {
		t.Fatalf("first %+v", p)
	}
}

func TestCandidatesRejectNonPowerOfTwoWindow(t *testing.T) {
	h := &format.ZstdFrameHeader{WindowLog: 20, WindowSize: (1 << 20) + (1 << 17)}
	if tiers := New().Candidates(h, 0); len(tiers) != 0 {
		t.Fatalf("expected no candidates, got %+v", tiers)
	}
}

func TestConcurrencyDoesNotChangeOutput(t *testing.T) {
	data := fixtures.Mixed(8 << 20)
	var one, four bytes.Buffer
	e1, _ := zstd.NewWriter(&one, zstd.WithEncoderConcurrency(1))
	e1.Write(data)
	e1.Close()
	e4, _ := zstd.NewWriter(&four, zstd.WithEncoderConcurrency(4))
	e4.Write(data)
	e4.Close()
	if !bytes.Equal(one.Bytes(), four.Bytes()) {
		t.Fatal("klauspost zstd output depends on concurrency; files made with concurrency > 1 will not be reproducible")
	}
}

// TestAllWriterCloseIdempotent proves a second Close on the single-segment
// (EncodeAll) writer path is a no-op instead of re-encoding and writing the
// frame a second time.
func TestAllWriterCloseIdempotent(t *testing.T) {
	var buf bytes.Buffer
	w, err := New().NewWriter(&buf, engine.ZstdParams{Level: 1, SingleSegment: true}, 5)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("hello"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	first := buf.Len()
	if err := w.Close(); err != nil || buf.Len() != first {
		t.Fatalf("second close: err=%v, wrote %d more bytes", err, buf.Len()-first)
	}
}

func TestRoundTrip(t *testing.T) {
	var params []engine.ZstdParams
	for l := 1; l <= 4; l++ {
		params = append(params,
			engine.ZstdParams{Level: l, Checksum: true},
			engine.ZstdParams{Level: l, Checksum: false, ContentSize: true, PledgedSize: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, SingleSegment: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, EncodeAll: true},
			engine.ZstdParams{Level: l, Checksum: true, Head: 8},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, Head: 8},
		)
	}
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}

// writeThenReadFrom is what containers/image (skopeo, podman, buildah)
// does with an uncompressed layer: the bytes peeked at to detect the
// source compression go through Write, the rest through Encoder.ReadFrom,
// which flushes what Write buffered as a block of its own before it
// streams.
func writeThenReadFrom(t *testing.T, level zstd.EncoderLevel, data []byte, head int) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(level))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data[:head]); err != nil {
		t.Fatal(err)
	}
	if _, err := enc.ReadFrom(bytes.NewReader(data[head:])); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func frameLength(t *testing.T, frame []byte) *format.ZstdFrameHeader {
	t.Helper()
	h, _, err := format.ZstdFrameLength(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCandidatesForFlushedHead(t *testing.T) {
	frame := writeThenReadFrom(t, zstd.SpeedBetterCompression, fixtures.Text(300000), 8)
	tiers := New().Candidates(frameLength(t, frame), 300000)
	if len(tiers) != 1 || len(tiers[0]) != 4 {
		t.Fatalf("%+v", tiers)
	}
	for _, p := range tiers[0] {
		if p.Head != 8 || p.EncodeAll || p.SingleSegment {
			t.Fatalf("candidate %+v, want head 8 and the streaming path", p)
		}
	}
	if p := tiers[0][0]; p.Level != 2 || p.WindowLog != 23 || !p.Checksum {
		t.Fatalf("first %+v", p)
	}

	// A frame of zeros from the fastest level starts with a 64 KiB RLE
	// block: a full block for level 1, a flushed head for the others.
	frame = zstdStream(t, make([]byte, 300000), zstd.WithEncoderLevel(zstd.SpeedFastest))
	tiers = New().Candidates(frameLength(t, frame), 300000)
	heads := map[int]int{}
	for _, p := range tiers[0] {
		heads[p.Level] = p.Head
	}
	if heads[1] != 0 || heads[2] != 64<<10 {
		t.Fatalf("heads by level %v, want 0 for level 1 and 65536 for the rest", heads)
	}
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

func TestHeadReproducesWriteThenReadFrom(t *testing.T) {
	data := fixtures.Mixed(600000)
	for _, tc := range []struct {
		level zstd.EncoderLevel
		head  int
	}{
		{zstd.SpeedDefault, 8},
		{zstd.SpeedBetterCompression, 8},
		{zstd.SpeedFastest, 40000}, // a head longer than one Feed write
	} {
		want := writeThenReadFrom(t, tc.level, data, tc.head)
		p := engine.ZstdParams{Level: int(tc.level), WindowLog: 23, Checksum: true, Head: tc.head}
		if tc.level == zstd.SpeedFastest {
			p.WindowLog = 22
		}
		var got bytes.Buffer
		w, err := New().NewWriter(&got, p, int64(len(data)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Feed(w, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("level %s head %d: %d bytes, want %d; head candidate does not reproduce Write then ReadFrom", tc.level, tc.head, got.Len(), len(want))
		}
	}
}

func TestNewWriterRejectsHeadWithOneShotPaths(t *testing.T) {
	for _, p := range []engine.ZstdParams{
		{Level: 2, Head: 8, EncodeAll: true, ContentSize: true, PledgedSize: true},
		{Level: 2, Head: 8, SingleSegment: true, ContentSize: true, PledgedSize: true},
	} {
		if _, err := New().NewWriter(io.Discard, p, 100); err == nil {
			t.Fatalf("%+v: no error; a head is flushed by the streaming writer only", p)
		}
	}
}
