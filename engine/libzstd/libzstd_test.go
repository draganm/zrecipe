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

func TestEndWithDataDropsEmptyLastBlock(t *testing.T) {
	data := fixtures.Mixed(3 << 20)
	a := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 0, PledgedSize: true, ContentSize: true, EndWithData: true}, data)
	b := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 0, PledgedSize: true, ContentSize: true, EndWithData: false}, data)
	if len(b) != len(a)+3 {
		t.Fatalf("len(a)=%d len(b)=%d, want len(b) == len(a)+3", len(a), len(b))
	}

	ha, _, err := format.ZstdFrameLength(bufioReader(a))
	if err != nil {
		t.Fatal(err)
	}
	if ha.EmptyLastBlock {
		t.Fatal("EndWithData: true reported EmptyLastBlock")
	}
	hb, _, err := format.ZstdFrameLength(bufioReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if !hb.EmptyLastBlock {
		t.Fatal("EndWithData: false did not report EmptyLastBlock")
	}

	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	for name, frame := range map[string][]byte{"a": a, "b": b} {
		back, err := dec.DecodeAll(frame, nil)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestCandidatesUseEmptyLastBlock(t *testing.T) {
	check := func(t *testing.T, h *format.ZstdFrameHeader, want bool) {
		t.Helper()
		for _, tier := range New().Candidates(h, 1000) {
			for _, p := range tier {
				if p.Workers == 1 && p.EndWithData {
					t.Fatalf("workers-1 candidate has EndWithData: true: %+v", p)
				}
				if p.Workers == 0 && p.EndWithData != want {
					t.Fatalf("workers-0 candidate EndWithData=%v, want %v: %+v", p.EndWithData, want, p)
				}
			}
		}
	}
	check(t, &format.ZstdFrameHeader{Checksum: true, HasContentSize: true, WindowLog: 21, EmptyLastBlock: true}, false)
	check(t, &format.ZstdFrameHeader{Checksum: true, HasContentSize: true, WindowLog: 21, EmptyLastBlock: false}, true)
}

func TestCandidatesLongForSingleSegment(t *testing.T) {
	tiers := New().Candidates(&format.ZstdFrameHeader{SingleSegment: true, HasContentSize: true, Checksum: true}, 1000)
	found := false
	for _, p := range tiers[1] {
		if p.Long && p.WindowLog == 27 {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("single-segment header should add tier 2 long candidates with WindowLog 27")
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
	params = append(params, engine.ZstdParams{Level: 3, Workers: 0, PledgedSize: true, ContentSize: true, EndWithData: true})
	params = append(params, engine.ZstdParams{Level: 5, Long: true, WindowLog: 27, PledgedSize: true, ContentSize: true, Checksum: true})
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
