//go:build cgo

package zlib

import (
	"bytes"
	"compress/flate"
	"io"
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

// TestLevelZeroStoredBlocksAreCanonical proves level 0 emits maximal
// (65535-byte) stored blocks, matching what real deflate implementations
// produce, rather than being capped short by the internal I/O buffer size.
func TestLevelZeroStoredBlocksAreCanonical(t *testing.T) {
	data := fixtures.Random(200000, 7)
	var buf bytes.Buffer
	w, err := New().NewWriter(&buf, engine.DeflateParams{Level: 0, Strategy: "default", WindowBits: 15, MemLevel: 8})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out := buf.Bytes()
	if len(out) < 5 {
		t.Fatalf("output too short: %d bytes", len(out))
	}
	length := int(out[1]) | int(out[2])<<8
	if length != 65535 {
		t.Fatalf("first stored block LEN = %d, want 65535 (out[0:5] = % x)", length, out[:5])
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
