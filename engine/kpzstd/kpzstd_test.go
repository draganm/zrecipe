package kpzstd

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
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

func TestRoundTrip(t *testing.T) {
	var params []engine.ZstdParams
	for l := 1; l <= 4; l++ {
		params = append(params,
			engine.ZstdParams{Level: l, Checksum: true},
			engine.ZstdParams{Level: l, Checksum: false, ContentSize: true, PledgedSize: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, SingleSegment: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, EncodeAll: true},
		)
	}
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}
