//go:build cgo

package zlib

import (
	"bytes"
	"compress/flate"
	"io"
	"reflect"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "zlib" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "1.3.2" {
		t.Fatalf("zlib version %q, want 1.3.2 from the flake", v)
	}
}

func TestCandidateTiers(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 0)
	if len(tiers) != 3 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 10 || tiers[0][0] != (engine.DeflateParams{Level: 9, Strategy: "default", WindowBits: 15, MemLevel: 8}) {
		t.Fatalf("tier 1: %+v", tiers[0])
	}
	if len(tiers[1]) != 9*4 {
		t.Fatalf("tier 2 has %d", len(tiers[1]))
	}
	if len(tiers[2]) != 9*(9*7-1) {
		t.Fatalf("tier 3 has %d", len(tiers[2]))
	}
}

func TestOutputIsValidDeflate(t *testing.T) {
	data := fixtures.Text(100000)
	for _, p := range []engine.DeflateParams{
		{Level: 0, Strategy: "default", WindowBits: 15, MemLevel: 8},
		{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8},
		{Level: 9, Strategy: "huffman_only", WindowBits: 15, MemLevel: 8},
		{Level: 3, Strategy: "rle", WindowBits: 9, MemLevel: 1},
	} {
		var buf bytes.Buffer
		w, err := New().NewWriter(&buf, p)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		back, err := io.ReadAll(flate.NewReader(&buf))
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("%+v: decode failed: %v", p, err)
		}
	}
}

func TestParamValidation(t *testing.T) {
	for _, p := range []engine.DeflateParams{
		{Level: 10}, {Level: -1}, {Level: 5, Strategy: "bogus"}, {Level: 5, WindowBits: 8}, {Level: 5, MemLevel: 10},
	} {
		if _, err := New().NewWriter(io.Discard, p); err == nil {
			t.Errorf("%+v: expected error", p)
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestPropagatesWriteError(t *testing.T) {
	fw := &failWriter{}
	w, err := New().NewWriter(fw, engine.DeflateParams{Level: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(fixtures.Random(1<<20, 9))
	if err := w.Close(); err == nil {
		t.Fatal("expected error")
	}
	if fw.n != 1 {
		t.Fatalf("kept writing after failure: %d writes", fw.n)
	}
}

// compressIn compresses data with p, handing it to the writer in writes of
// at most chunk bytes (the whole of data in one Write when chunk is 0).
func compressIn(t *testing.T, p engine.DeflateParams, data []byte, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := New().NewWriter(&buf, p)
	if err != nil {
		t.Fatal(err)
	}
	for len(data) > 0 {
		n := len(data)
		if chunk > 0 && n > chunk {
			n = chunk
		}
		if _, err := w.Write(data[:n]); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// storedBlockLens walks a raw deflate stream that consists only of stored
// blocks and returns their LEN fields in order.
func storedBlockLens(t *testing.T, out []byte) []int {
	t.Helper()
	var lens []int
	for {
		if len(out) < 5 {
			t.Fatalf("truncated stored-block header: % x", out)
		}
		if out[0]&0x06 != 0 {
			t.Fatalf("block type %d is not stored", out[0]>>1&3)
		}
		n := int(out[1]) | int(out[2])<<8
		if nlen := int(out[3]) | int(out[4])<<8; nlen != ^n&0xffff {
			t.Fatalf("LEN %d and NLEN %d disagree", n, nlen)
		}
		if len(out) < 5+n {
			t.Fatalf("stored block LEN %d exceeds the %d remaining bytes", n, len(out)-5)
		}
		lens = append(lens, n)
		final := out[0]&1 == 1
		out = out[5+n:]
		if final {
			if len(out) != 0 {
				t.Fatalf("%d bytes after the final block", len(out))
			}
			return lens
		}
	}
}

var levelZero = engine.DeflateParams{Level: 0, Strategy: "default", WindowBits: 15, MemLevel: 8}

// TestLevelZeroStoredBlocksAreCanonical proves level 0 emits maximal
// (65535-byte) stored blocks, matching what real deflate implementations
// produce, rather than blocks capped short by the internal I/O buffers or
// by the size of the caller's Writes. zlib sizes a stored block from the
// input it can see in one deflate call, so without input batching 32 KiB
// writes yield 32768-byte blocks (issue #1).
func TestLevelZeroStoredBlocksAreCanonical(t *testing.T) {
	data := fixtures.Random(200000, 7)
	ref := compressIn(t, levelZero, data, 0)
	lens := storedBlockLens(t, ref)
	want := []int{65535, 65535, 65535, 200000 - 3*65535}
	if !reflect.DeepEqual(lens, want) {
		t.Fatalf("one Write: stored block lengths %v, want %v", lens, want)
	}
	for _, chunk := range []int{1 << 10, 32 << 10, 100000} {
		if got := compressIn(t, levelZero, data, chunk); !bytes.Equal(got, ref) {
			t.Errorf("%d-byte writes: stored block lengths %v, want %v", chunk, storedBlockLens(t, got), want)
		}
	}
}

// TestOutputIndependentOfWriteSize proves the stream depends only on the
// content and parameters, never on how the content was split across
// Writes: Analyze verifies a candidate and Recompress rebuilds the file
// from readers of different granularity.
func TestOutputIndependentOfWriteSize(t *testing.T) {
	inputs := map[string][]byte{"random-200k": fixtures.Random(200000, 7), "mixed-300k": fixtures.Mixed(300 << 10)}
	for name, data := range inputs {
		for _, level := range []int{0, 1, 6, 9} {
			p := engine.DeflateParams{Level: level, Strategy: "default", WindowBits: 15, MemLevel: 8}
			ref := compressIn(t, p, data, 0)
			for _, chunk := range []int{1 << 10, 32 << 10, 100000} {
				if got := compressIn(t, p, data, chunk); !bytes.Equal(got, ref) {
					t.Errorf("%s level %d: %d-byte writes give %d bytes, one Write gives %d", name, level, chunk, len(got), len(ref))
				}
			}
		}
	}
}

func TestRoundTripTier1And2(t *testing.T) {
	e := New()
	tiers := e.Candidates(&format.GzipHeader{}, 0)
	enginetest.RoundTripDeflate(t, e, append(tiers[0], tiers[1]...), fixtures.Small())
}

func TestRoundTripTier3Sample(t *testing.T) {
	e := New()
	var sample []engine.DeflateParams
	for _, l := range []int{1, 6, 9} {
		for _, ml := range []int{1, 5, 9} {
			for _, wb := range []int{9, 12, 15} {
				sample = append(sample, engine.DeflateParams{Level: l, Strategy: "default", WindowBits: wb, MemLevel: ml})
			}
		}
	}
	enginetest.RoundTripDeflate(t, e, sample, fixtures.Small()[2:4])
}
