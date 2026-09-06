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
	// Header with content size: pledged is fixed to true. Window 21 on a
	// 40 MiB input is the default window of levels 3 to 8. The first tier
	// is the single-thread path only: the job-based path is the rarer
	// producer and costs a whole first job to rule out, so it waits in
	// the second tier with the other unlikely candidates.
	tiers := New().Candidates(&format.ZstdFrameHeader{Checksum: true, HasContentSize: true, WindowLog: 21}, 40<<20)
	if len(tiers) != 2 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 6 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	for _, p := range tiers[0] {
		if p.Level < 3 || p.Level > 8 || p.Workers != 0 {
			t.Fatalf("%+v in tier 1 for window 21", p)
		}
	}
	jobs := 0
	for _, p := range tiers[1] {
		if p.Workers == 1 && p.WindowLog == 0 && p.Level >= 3 && p.Level <= 8 {
			jobs++
		}
	}
	if jobs != 6 {
		t.Fatalf("tier 2 has %d job-based candidates of levels 3 to 8, want 6", jobs)
	}
	first := tiers[0][0]
	if first.Level != 3 || !first.Checksum || !first.ContentSize || !first.PledgedSize || first.Workers != 0 || first.WindowLog != 0 {
		t.Fatalf("first candidate %+v", first)
	}
	// Without content size both pledged values are tried; window 27 is
	// level 22's, so tier 1 is empty and tier 2 carries level 22, the
	// explicit-window variants of the tier 1 levels and long mode. The
	// input is longer than the window, or a pledged size would shrink it.
	tiers = New().Candidates(&format.ZstdFrameHeader{WindowLog: 27}, 200<<20)
	if len(tiers[0]) != 0 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	var dflt, explicit, long int
	for _, p := range tiers[1] {
		switch {
		case p.Long:
			long++
		case p.WindowLog == 27:
			explicit++
		default:
			dflt++
			if p.Level != 22 {
				t.Fatalf("default-window candidate at level %d for window 27", p.Level)
			}
		}
	}
	if dflt != 2*2 || explicit != 19*2*2 || long == 0 {
		t.Fatalf("tier 2: %d default-window, %d explicit-window, %d long", dflt, explicit, long)
	}
	for _, tier := range tiers[:1] {
		for _, p := range tier {
			if p.Workers != 0 {
				t.Fatalf("job-based candidate in tier 1: %+v", p)
			}
		}
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
		tiers := New().Candidates(h, 40<<20)
		if len(enginetest.Flatten(tiers)) == 0 {
			t.Fatal("no candidates")
		}
		for _, tier := range tiers {
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

// firstOutputAt feeds data to a writer in FeedSize writes and returns the
// input position at which the writer first wrote output.
func firstOutputAt(t *testing.T, p engine.ZstdParams, data []byte) int64 {
	t.Helper()
	var (
		fed   int64
		first int64 = -1
	)
	w, err := New().NewWriter(writerFunc(func(b []byte) (int, error) {
		if first < 0 && len(b) > 0 {
			first = fed
		}
		return len(b), nil
	}), p, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off < len(data); off += engine.FeedSize {
		end := min(off+engine.FeedSize, len(data))
		if _, err := w.Write(data[off:end]); err != nil {
			t.Fatal(err)
		}
		fed = int64(end)
		if first >= 0 {
			break
		}
	}
	if first < 0 {
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return int64(len(data))
	}
	w.Close()
	return first
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

// TestBufferedPredictsFirstOutput: a job-based candidate jobWriter drives
// shows output within a chunk of the start and reports nothing buffered;
// one libzstd's own compressor drives (long distance matching, a job over
// maxEmulatedJob) shows nothing until its first job is full, and Buffered
// says how much that is: a job of 2^max(20, windowLog+2) bytes, for the
// window the level and the pledged size resolve to, or the whole input
// when that is shorter. The output of such a job must still surface within
// a batch or two of the job boundary, which is what the elimination
// relies on when it drops candidates at its limit. The single-thread path
// streams from the first block.
func TestBufferedPredictsFirstOutput(t *testing.T) {
	const slack = 3 * 128 << 10
	mixed := fixtures.Mixed(2 << 20)
	for _, tc := range []struct {
		p    engine.ZstdParams
		size int64
		data []byte
		want int64 // Buffered
	}{
		{engine.ZstdParams{Level: 1, Workers: 1}, 40 << 20, nil, 0},                                       // job 2 MiB, emulated
		{engine.ZstdParams{Level: 19, Workers: 1}, 40 << 20, nil, 0},                                      // job 32 MiB, emulated
		{engine.ZstdParams{Level: 19, Workers: 1, WindowLog: 18}, 2 << 20, mixed, 0},                      // job 1 MiB, emulated
		{engine.ZstdParams{Level: 3, Workers: 1, PledgedSize: true, ContentSize: true}, 40 << 20, nil, 0}, // job 8 MiB, emulated
		{engine.ZstdParams{Level: 20, Workers: 1, WindowLog: 25}, 40 << 20, nil, 128 << 20},               // job 128 MiB, libzstd's own: the whole input
		{engine.ZstdParams{Level: 19, Workers: 1, Long: true, WindowLog: 27}, 40 << 20, nil, 64 << 20},    // cycle log 23 of chain log 24, btultra2
		{engine.ZstdParams{Level: 3, Workers: 1, Long: true, WindowLog: 27}, 40 << 20, mixed, 2 << 20},    // cycle log 16 of chain log 16: the 2 MiB floor
		{engine.ZstdParams{Level: 19, Workers: 0}, 4 << 20, nil, 0},
		{engine.ZstdParams{Level: 3, Workers: 0, EndWithData: true}, 4 << 20, nil, 0},
	} {
		got := New().Buffered(tc.p, tc.size)
		if got != tc.want {
			t.Errorf("%+v size %d: Buffered %d, want %d", tc.p, tc.size, got, tc.want)
			continue
		}
		data := tc.data
		if data == nil {
			data = make([]byte, tc.size)
		}
		at := firstOutputAt(t, tc.p, data)
		lo, hi := int64(0), int64(jobChunk+engine.FeedSize)
		if tc.want > 0 {
			lo = min(tc.want, int64(len(data)))
			hi = lo + slack
		}
		if at < lo || at > hi {
			t.Errorf("%+v size %d: first output at %d, want within [%d, %d]", tc.p, tc.size, at, lo, hi)
		}
	}
}

// TestCandidatesProduceTheHeaderWindow: every candidate offered for a
// header writes that header's window descriptor. The window a level
// resolves to is a function of the level, the pledged size and an explicit
// window, so candidates that would write a different one are left out
// rather than started and killed at byte six, which for the job-based
// path means after a whole first job.
func TestCandidatesProduceTheHeaderWindow(t *testing.T) {
	// 3 MiB: longer than the 2 MiB window of the second header, or libzstd
	// would write a single-segment frame for the pledged size.
	data := fixtures.Mixed(3 << 20)
	for _, h := range []*format.ZstdFrameHeader{
		{WindowLog: 23, WindowSize: 1 << 23, Checksum: true},                       // klauspost's default window, size unknown
		{WindowLog: 21, WindowSize: 1 << 21, Checksum: true, HasContentSize: true}, // zstd -3 on a file
	} {
		cands := enginetest.Flatten(New().Candidates(h, int64(len(data))))
		if len(cands) == 0 {
			t.Fatalf("no candidates for %+v", h)
		}
		for _, p := range cands {
			got, err := format.ParseZstdFrameHeader(bufioReader(enginetest.Zstd(t, New(), p, data)))
			if err != nil {
				t.Fatal(err)
			}
			if got.SingleSegment || got.WindowLog != h.WindowLog || got.WindowSize != h.WindowSize {
				t.Errorf("%+v writes window log %d size %d for a header with %d %d", p, got.WindowLog, got.WindowSize, h.WindowLog, h.WindowSize)
			}
		}
		t.Logf("%d candidates for window log %d", len(cands), h.WindowLog)
	}

	// A pledged size that fits the window makes libzstd write a
	// single-segment frame: for a header with a window descriptor those
	// candidates are out, for a single-segment header they are the only
	// ones. 1 MiB of content: level 1's 512 KiB window does not hold it,
	// the other levels' windows do.
	small := fixtures.Mixed(1 << 20)
	for _, p := range enginetest.Flatten(New().Candidates(&format.ZstdFrameHeader{WindowLog: 20, WindowSize: 1 << 20, Checksum: true, HasContentSize: true}, int64(len(small)))) {
		got, err := format.ParseZstdFrameHeader(bufioReader(enginetest.Zstd(t, New(), p, small)))
		if err != nil {
			t.Fatal(err)
		}
		if got.SingleSegment || got.WindowLog != 20 {
			t.Errorf("%+v writes a single-segment frame or window log %d for a window-20 header", p, got.WindowLog)
		}
	}
	single := enginetest.Flatten(New().Candidates(&format.ZstdFrameHeader{SingleSegment: true, WindowSize: 1 << 20, Checksum: true, HasContentSize: true, ContentSize: 1 << 20}, int64(len(small))))
	if len(single) == 0 {
		t.Fatal("no candidates for a single-segment header")
	}
	for _, p := range single {
		got, err := format.ParseZstdFrameHeader(bufioReader(enginetest.Zstd(t, New(), p, small)))
		if err != nil {
			t.Fatal(err)
		}
		if !got.SingleSegment {
			t.Errorf("%+v writes a window descriptor for a single-segment header", p)
		}
	}

	// A window descriptor with a mantissa cannot come from libzstd.
	if n := len(enginetest.Flatten(New().Candidates(&format.ZstdFrameHeader{WindowLog: 23, WindowSize: 1<<23 + 1<<20}, 1<<20))); n != 0 {
		t.Fatalf("%d candidates for a window with a mantissa, want none", n)
	}

	// The explicit-window variants repeat only the levels whose default
	// window is not the header's: 17 to 19 resolve to 23 already.
	tiers := New().Candidates(&format.ZstdFrameHeader{WindowLog: 23, WindowSize: 1 << 23}, 40<<20)
	explicit := 0
	for _, p := range tiers[1] {
		if p.WindowLog == 23 && !p.Long {
			explicit++
			if p.Level >= 17 && p.Level <= 19 {
				t.Errorf("level %d repeated with its own default window: %+v", p.Level, p)
			}
		}
	}
	if explicit != 16*2*2 {
		t.Fatalf("%d explicit-window candidates, want 16 levels x workers x pledged", explicit)
	}
}

// TestJobBasedWriterStreamsFromTheFirstChunk: the job-based path shows its
// first output once a chunk of its first job is compressed, not once the
// job is full, so the elimination can drop a wrong candidate after 512 KiB
// instead of after a whole job at that level.
func TestJobBasedWriterStreamsFromTheFirstChunk(t *testing.T) {
	data := fixtures.Mixed(3 << 20)
	p := engine.ZstdParams{Level: 19, Workers: 1, Checksum: true}
	if at := firstOutputAt(t, p, data); at > jobChunk+engine.FeedSize {
		t.Fatalf("first output at %d, want within one chunk (%d) of the start", at, jobChunk)
	}
}

// TestJobBasedWriterMatchesLibzstd: whatever the writer does to show output
// early, its bytes are exactly what libzstd's own job-based compressor
// writes, over inputs shorter than a job, longer than several jobs, a
// known size small enough for libzstd to fall back to one thread, and
// empty.
func TestJobBasedWriterMatchesLibzstd(t *testing.T) {
	big := fixtures.Text(3 << 20)
	for _, tc := range []struct {
		name string
		p    engine.ZstdParams
		data []byte
	}{
		{"one job", engine.ZstdParams{Level: 3, Workers: 1, Checksum: true}, fixtures.Mixed(300 << 10)},
		{"one job pledged", engine.ZstdParams{Level: 7, Workers: 1, ContentSize: true, PledgedSize: true}, fixtures.Mixed(3 << 20)},
		{"one job, chunk aligned", engine.ZstdParams{Level: 3, Workers: 1}, fixtures.Text(1 << 20)},
		{"three jobs", engine.ZstdParams{Level: 19, Workers: 1, WindowLog: 18, Checksum: true}, big},
		{"three jobs pledged", engine.ZstdParams{Level: 1, Workers: 1, Checksum: true, ContentSize: true, PledgedSize: true}, big},
		{"job aligned", engine.ZstdParams{Level: 3, Workers: 1, WindowLog: 18}, fixtures.Text(2 << 20)},
		{"small known size", engine.ZstdParams{Level: 3, Workers: 1, Checksum: true, ContentSize: true, PledgedSize: true}, fixtures.Mixed(300 << 10)},
		{"tiny", engine.ZstdParams{Level: 19, Workers: 1}, []byte("hello")},
		{"empty", engine.ZstdParams{Level: 3, Workers: 1, Checksum: true}, nil},
		{"empty pledged", engine.ZstdParams{Level: 3, Workers: 1, ContentSize: true, PledgedSize: true}, nil},
		{"negative level", engine.ZstdParams{Level: -3, Workers: 1}, fixtures.Mixed(3 << 20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var direct bytes.Buffer
			w, err := newDirectWriter(&direct, tc.p, int64(len(tc.data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Feed(w, bytes.NewReader(tc.data)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			w, err = New().NewWriter(&got, tc.p, int64(len(tc.data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := engine.Feed(w, bytes.NewReader(tc.data)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), direct.Bytes()) {
				t.Fatalf("%d bytes, libzstd's job-based compressor writes %d; they differ", got.Len(), direct.Len())
			}
		})
	}
}
