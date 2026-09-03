//go:build cgo

package libzstd

import (
	"bufio"
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "libzstd" || e.Format() != engine.FormatZstd {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "1.5.7" {
		t.Fatalf("libzstd version %q, want 1.5.7 from the flake", v)
	}
}

func TestCandidateTiers(t *testing.T) {
	// Header with content size: pledged is fixed to true.
	tiers := New().Candidates(&format.ZstdFrameHeader{Checksum: true, HasContentSize: true, WindowLog: 21}, 1000)
	if len(tiers) != 2 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 19*2 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	first := tiers[0][0]
	if first.Level != 3 || !first.Checksum || !first.ContentSize || !first.PledgedSize || first.Workers != 0 || first.WindowLog != 0 {
		t.Fatalf("first candidate %+v", first)
	}
	// Without content size both pledged values are tried.
	tiers = New().Candidates(&format.ZstdFrameHeader{WindowLog: 27}, 1000)
	if len(tiers[0]) != 19*2*2 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	long := 0
	for _, p := range tiers[1] {
		if p.Long {
			long++
		}
	}
	if long == 0 {
		t.Fatal("window log 27 should add long mode candidates")
	}
}

func TestOutputDecodes(t *testing.T) {
	data := fixtures.Mixed(400000)
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	for _, p := range []engine.ZstdParams{
		{Level: 1, Checksum: true, ContentSize: true, PledgedSize: true},
		{Level: 19, Checksum: false},
		{Level: 3, Workers: 1, Checksum: true},
		{Level: -5},
		{Level: 5, Long: true, WindowLog: 27},
	} {
		frame := enginetest.Zstd(t, New(), p, data)
		back, err := dec.DecodeAll(frame, nil)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("%+v: %v", p, err)
		}
	}
}

func TestPledgedSizeSetsHeader(t *testing.T) {
	data := fixtures.Text(5000)
	frame := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, ContentSize: true, PledgedSize: true}, data)
	h, err := format.ParseZstdFrameHeader(bufioReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasContentSize || h.ContentSize != 5000 || !h.SingleSegment {
		t.Fatalf("%+v", h)
	}
	frame = enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3}, data)
	h, _ = format.ParseZstdFrameHeader(bufioReader(frame))
	if h.HasContentSize || h.SingleSegment {
		t.Fatalf("streamed: %+v", h)
	}
}

func TestWorkersOutputIndependentOfCount(t *testing.T) {
	data := fixtures.Mixed(8 << 20)
	one := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 1}, data)
	four := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 4}, data)
	if !bytes.Equal(one, four) {
		t.Fatal("libzstd output depends on the number of workers; the Workers parameter must record the real count")
	}
	zero := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 0}, data)
	t.Logf("single-thread and job-based outputs equal: %v", bytes.Equal(zero, one))
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestPropagatesWriteError(t *testing.T) {
	fw := &failWriter{}
	w, err := New().NewWriter(fw, engine.ZstdParams{Level: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(fixtures.Random(2<<20, 5))
	if err := w.Close(); err == nil {
		t.Fatal("expected error")
	}
	if fw.n != 1 {
		t.Fatalf("kept writing after failure: %d writes", fw.n)
	}
}

func TestRoundTrip(t *testing.T) {
	var params []engine.ZstdParams
	for _, l := range []int{1, 3, 9, 19, -3} {
		for _, w := range []int{0, 1} {
			params = append(params,
				engine.ZstdParams{Level: l, Workers: w, Checksum: true, ContentSize: true, PledgedSize: true},
				engine.ZstdParams{Level: l, Workers: w},
			)
		}
	}
	params = append(params, engine.ZstdParams{Level: 5, Long: true, WindowLog: 27, Checksum: true})
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
