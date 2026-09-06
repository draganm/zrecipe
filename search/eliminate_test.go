package search

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

// countingEngine is a deflate engine for elimination tests. Each writer
// copies the content verbatim, corrupts one byte once divergeAt[level]
// input bytes have gone by (0 means never), buffers block bytes before it
// writes anything when block is set (as pigz and pgzip do), and records
// what it was fed, the size of every write and how many writers are open.
type countingEngine struct {
	divergeAt map[int]int64
	block     int

	mu            sync.Mutex
	fed           map[int]int64
	writes        map[int][]int
	open, maxOpen int
}

func newCountingEngine(divergeAt map[int]int64, block int) *countingEngine {
	return &countingEngine{divergeAt: divergeAt, block: block, fed: map[int]int64{}, writes: map[int][]int{}}
}

func (f *countingEngine) Name() string          { return "counting" }
func (f *countingEngine) Version() string       { return "1" }
func (f *countingEngine) Format() engine.Format { return engine.FormatGzip }
func (f *countingEngine) Candidates(*format.GzipHeader, int64) [][]engine.DeflateParams {
	return nil
}

func (f *countingEngine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	f.mu.Lock()
	f.open++
	f.maxOpen = max(f.maxOpen, f.open)
	f.mu.Unlock()
	return &countingWriter{f: f, w: w, level: p.Level, at: f.divergeAt[p.Level]}, nil
}

func (f *countingEngine) fedBytes(level int) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fed[level]
}

type countingWriter struct {
	f      *countingEngine
	w      io.Writer
	level  int
	at     int64
	pos    int64
	buf    []byte
	closed bool
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.f.mu.Lock()
	c.f.fed[c.level] += int64(len(p))
	c.f.writes[c.level] = append(c.f.writes[c.level], len(p))
	c.f.mu.Unlock()
	out := make([]byte, len(p))
	copy(out, p)
	if c.at > 0 && c.at >= c.pos && c.at < c.pos+int64(len(p)) {
		out[c.at-c.pos] ^= 0xff
	}
	c.pos += int64(len(p))
	if c.f.block == 0 {
		if _, err := c.w.Write(out); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	c.buf = append(c.buf, out...)
	for len(c.buf) >= c.f.block {
		if _, err := c.w.Write(c.buf[:c.f.block]); err != nil {
			return 0, err
		}
		c.buf = c.buf[c.f.block:]
	}
	return len(p), nil
}

func (c *countingWriter) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	c.f.mu.Lock()
	c.f.open--
	c.f.mu.Unlock()
	if len(c.buf) > 0 {
		_, err := c.w.Write(c.buf)
		c.buf = nil
		return err
	}
	return nil
}

// identityInput is an Input whose reference payload is the content itself
// followed by a trailer, the shape countingEngine reproduces.
func identityInput(t *testing.T, content []byte, concurrent bool) *Input {
	t.Helper()
	ref := append(append([]byte{}, content...), "TRAILER"...)
	sp := NewSpool(t.TempDir(), 1<<20)
	if _, err := sp.Write(content); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	return &Input{
		Format:           format.Gzip,
		Payload:          func(off int64) (io.Reader, error) { return bytes.NewReader(ref[off:]), nil },
		Concurrent:       concurrent,
		Trailer:          []byte("TRAILER"),
		Spool:            sp,
		UncompressedSize: int64(len(content)),
	}
}

func filler(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%23)
	}
	return b
}

func TestEliminateSettlesSmallInputComplete(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Eliminate(context.Background(), input(t, []byte("small content"), 2, true), candidates(e, 1, 2, 3), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || !res.Verified || res.Tried != 3 {
		t.Fatalf("res %+v", res)
	}
}

func TestEliminateSettlesLargeInputAfterMargin(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 1, 3: 1}, 0)
	content := filler(1 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2, 3), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || res.Verified {
		t.Fatalf("res %+v", res)
	}
	// The losers died in their first write, placed at that write's end, so
	// the second write of the first window is the margin.
	if got := f.fedBytes(2); got != FirstWindow {
		t.Fatalf("winner fed %d bytes, want %d", got, FirstWindow)
	}
	if got := f.fedBytes(1); got != engine.FeedSize {
		t.Fatalf("loser fed %d bytes, want one write", got)
	}
}

func TestEliminateTwoAgreeingCandidates(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 300 << 10}, 0)
	content := filler(2 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || res.Verified {
		t.Fatalf("res %+v", res)
	}
	// Both survive 64 KiB and 256 KiB; the loser dies inside the 1 MiB
	// window and the winner, at 1 MiB, is well past the margin.
	if got := f.fedBytes(2); got != 1<<20 {
		t.Fatalf("winner fed %d bytes, want 1 MiB", got)
	}
	if got := f.fedBytes(1); got < 300<<10 || got > 1<<20 {
		t.Fatalf("loser fed %d bytes", got)
	}
}

func TestEliminateLoneSurvivorIsFedToTheMargin(t *testing.T) {
	// The loser dies 8 KiB before the end of the 256 KiB window, so the
	// survivor is short of the margin there and is fed one more write.
	f := newCountingEngine(map[int]int64{1: 248 << 10}, 0)
	content := filler(2 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || res.Verified {
		t.Fatalf("res %+v", res)
	}
	if got := f.fedBytes(2); got != 256<<10+engine.FeedSize {
		t.Fatalf("winner fed %d bytes, want %d", got, 256<<10+engine.FeedSize)
	}
}

func TestEliminateFallsBackAboveTheCap(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 200 << 10, 2: 200 << 10, 3: 200 << 10}, 0)
	content := filler(2 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2, 3, 4), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 3 || !res.Verified {
		t.Fatalf("res %+v", res)
	}
	// Three tested survivors of the first window trip the cap: two kept,
	// one in flight, and Run afterwards opens one at a time.
	if f.maxOpen > MaxLockstep+1 {
		t.Fatalf("%d writers were open at once", f.maxOpen)
	}
}

func TestEliminateFallbackCoversUnreachedCandidates(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 200 << 10, 2: 200 << 10, 3: 200 << 10, 4: 100 << 10}, 0)
	content := filler(2 << 20)
	// The cap trips after level 3 survives the first window; levels 4 and
	// 5 were never fed, and 5 is the producer.
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2, 3, 4, 5), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 4 || !res.Verified {
		t.Fatalf("res %+v", res)
	}
	if res.Tried < 5 {
		t.Fatalf("tried %d", res.Tried)
	}
}

func TestEliminateUntestedCandidateWaitsForItsBlock(t *testing.T) {
	// The producer buffers 512 KiB before it writes anything, like a pgzip
	// candidate filling its first block; the others die at once. The
	// elimination cannot settle before the window reaches the block, and
	// settles right after.
	f := newCountingEngine(map[int]int64{1: 1, 2: 1}, 512<<10)
	content := filler(4 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2, 3), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 2 || res.Verified {
		t.Fatalf("res %+v", res)
	}
	if got := f.fedBytes(3); got != 1<<20 {
		t.Fatalf("producer fed %d bytes, want 1 MiB", got)
	}
}

func TestEliminateBufferedCandidatesCompleteSmallInput(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 100}, 512<<10)
	content := filler(100 << 10)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || !res.Verified {
		t.Fatalf("res %+v", res)
	}
}

func TestEliminateEmptyContent(t *testing.T) {
	// Nothing is fed; every candidate is closed at once and its trailer
	// compared, so both match and the first wins.
	f := newCountingEngine(nil, 0)
	res, err := Eliminate(context.Background(), identityInput(t, nil, true), candidates(f, 1, 2), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 0 || !res.Verified || res.Tried != 2 {
		t.Fatalf("res %+v", res)
	}
}

func TestEliminateNoMatch(t *testing.T) {
	e := &fakeDeflate{}
	_, err := Eliminate(context.Background(), input(t, filler(1<<20), 9, true), candidates(e, 1, 2, 3), 2)
	if !errors.Is(err, ErrNoMatch) || !strings.Contains(err.Error(), "tried 3 candidates") {
		t.Fatalf("got %v", err)
	}
}

func TestEliminateCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Eliminate(ctx, input(t, filler(1<<20), 2, true), candidates(&fakeDeflate{}, 1, 2), 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestEliminateTrailerMismatch(t *testing.T) {
	in := input(t, []byte("content"), 2, true)
	in.Trailer = []byte("OTHER")
	_, err := Eliminate(context.Background(), in, candidates(&fakeDeflate{}, 2), 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
}

func TestEliminateReportsNonMismatchError(t *testing.T) {
	boom := errors.New("engine exploded")
	_, err := Eliminate(context.Background(), input(t, filler(1<<20), 2, true), candidates(&errDeflate{err: boom}, 1, 2), 1)
	if !errors.Is(err, ErrNoMatch) || !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("got %v", err)
	}
}

func TestEliminateSequentialInput(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 300 << 10}, 0)
	content := filler(2 << 20)
	res, err := Eliminate(context.Background(), identityInput(t, content, false), candidates(f, 1, 2), 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || res.Verified {
		t.Fatalf("res %+v", res)
	}
	if f.maxOpen != 2 {
		t.Fatalf("%d writers were open at once, want 2 (one at a time is fed, both stay alive)", f.maxOpen)
	}
}

func TestEliminateFeedsFixedSizeWrites(t *testing.T) {
	f := newCountingEngine(map[int]int64{1: 1536 << 10}, 0)
	content := filler(2<<20 + 100)
	res, err := Eliminate(context.Background(), identityInput(t, content, true), candidates(f, 1, 2), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 1 || !res.Verified {
		t.Fatalf("res %+v", res)
	}
	// The winner was fed across four windows to the end: every write is a
	// full FeedSize except the last, exactly Feed's shape over the whole.
	writes := f.writes[2]
	for i, n := range writes {
		want := engine.FeedSize
		if i == len(writes)-1 {
			want = 100
		}
		if n != want {
			t.Fatalf("write %d of %d is %d bytes, want %d", i, len(writes), n, want)
		}
	}
	if len(writes) != 2<<20/engine.FeedSize+1 {
		t.Fatalf("%d writes", len(writes))
	}
}
