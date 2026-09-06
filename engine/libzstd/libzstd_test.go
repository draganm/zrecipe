//go:build cgo

package libzstd

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

// compressIn compresses data with p, handing it to the writer in writes of
// at most chunk bytes (the whole of data in one Write when chunk is 0).
func compressIn(t *testing.T, p engine.ZstdParams, data []byte, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := New().NewWriter(&buf, p, int64(len(data)))
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

// TestOutputIndependentOfWriteSize proves the frame depends only on the
// content and parameters, never on how the content was split across
// Writes. Without input batching, end_with_data with no pledged size
// handed whatever the last Write held to ZSTD_e_end; for content that
// arrived in one Write that was also the first call, which makes libzstd
// pledge the size itself and tune its parameters to it (issue #1).
func TestOutputIndependentOfWriteSize(t *testing.T) {
	small := fixtures.Text(64 << 10) // fits one input batch
	big := fixtures.Mixed(300 << 10) // spans several
	for _, p := range []engine.ZstdParams{
		{Level: 3, EndWithData: true},
		{Level: 1, EndWithData: true},
		{Level: 19, EndWithData: true},
		{Level: -3, EndWithData: true},
		{Level: 3, EndWithData: false},
		{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true, EndWithData: true},
		{Level: 3, Workers: 1},
	} {
		for name, data := range map[string][]byte{"text-64k": small, "mixed-300k": big} {
			ref := compressIn(t, p, data, 0)
			for _, chunk := range []int{1 << 10, 32 << 10, 100000} {
				if got := compressIn(t, p, data, chunk); !bytes.Equal(got, ref) {
					t.Errorf("%s %+v: %d-byte writes give %d bytes, one Write gives %d", name, p, chunk, len(got), len(ref))
				}
			}
		}
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

// TestCandidatesNoneForFlushedHead: a first block flushed before it was
// full cannot come from libzstd, whose streaming paths cut full blocks
// until the end of the input, so there is nothing to try. Every candidate
// would otherwise die at that block, the job-based ones only after filling
// their first job.
func TestCandidatesNoneForFlushedHead(t *testing.T) {
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	data := fixtures.Text(300000)
	enc.Write(data[:8])
	enc.Flush()
	enc.Write(data[8:])
	enc.Close()
	h, _, err := format.ZstdFrameLength(bufio.NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(enginetest.Flatten(New().Candidates(h, int64(len(data))))); n != 0 {
		t.Fatalf("%d candidates for a frame with a flushed 8-byte head, want none", n)
	}
}
