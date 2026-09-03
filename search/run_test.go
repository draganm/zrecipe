package search

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// fakeDeflate "compresses" by prefixing the content with its level byte.
type fakeDeflate struct {
	mu    sync.Mutex
	calls []int
}

func (f *fakeDeflate) Name() string          { return "fake" }
func (f *fakeDeflate) Version() string       { return "1" }
func (f *fakeDeflate) Format() engine.Format { return engine.FormatGzip }
func (f *fakeDeflate) Candidates(*format.GzipHeader, int64) [][]engine.DeflateParams {
	return nil
}

type prefixWriter struct {
	w     io.Writer
	level int
	first bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if !p.first {
		p.first = true
		if _, err := p.w.Write([]byte{byte(p.level)}); err != nil {
			return 0, err
		}
	}
	return p.w.Write(b)
}

func (p *prefixWriter) Close() error { return nil }

func (f *fakeDeflate) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p.Level)
	f.mu.Unlock()
	return &prefixWriter{w: w, level: p.Level}, nil
}

func input(t *testing.T, content []byte, level int, concurrent bool) *Input {
	t.Helper()
	ref := append([]byte{byte(level)}, content...)
	ref = append(ref, "TRAILER"...)
	sp := NewSpool(t.TempDir(), 1<<20)
	sp.Write(content)
	t.Cleanup(func() { sp.Close() })
	return &Input{
		Format:           format.Gzip,
		Payload:          func() (io.Reader, error) { return bytes.NewReader(ref), nil },
		Concurrent:       concurrent,
		Trailer:          []byte("TRAILER"),
		Spool:            sp,
		UncompressedSize: int64(len(content)),
	}
}

// errDeflate fails every NewWriter call with a fixed, non-mismatch error.
type errDeflate struct {
	err error
}

func (e *errDeflate) Name() string          { return "err" }
func (e *errDeflate) Version() string       { return "1" }
func (e *errDeflate) Format() engine.Format { return engine.FormatGzip }
func (e *errDeflate) Candidates(*format.GzipHeader, int64) [][]engine.DeflateParams {
	return nil
}
func (e *errDeflate) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	return nil, e.err
}

// cancelDeflate fails every NewWriter call with context.Canceled, simulating
// a candidate that was cancelled rather than one whose output mismatched.
type cancelDeflate struct{}

func (c *cancelDeflate) Name() string          { return "cancel" }
func (c *cancelDeflate) Version() string       { return "1" }
func (c *cancelDeflate) Format() engine.Format { return engine.FormatGzip }
func (c *cancelDeflate) Candidates(*format.GzipHeader, int64) [][]engine.DeflateParams {
	return nil
}
func (c *cancelDeflate) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	return nil, context.Canceled
}

func candidates(e engine.Engine, levels ...int) []Candidate {
	var out []Candidate
	for _, l := range levels {
		p := engine.DeflateParams{Level: l}
		out = append(out, Candidate{Engine: e, Deflate: &p})
	}
	return out
}

func TestRunFindsMatchSequentially(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, []byte("content"), 3, false), candidates(e, 0, 1, 2, 3, 4), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 3 || res.Candidate.Deflate.Level != 3 || res.Tried != 4 {
		t.Fatalf("%+v", res)
	}
}

func TestRunFindsMatchInParallel(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, bytes.Repeat([]byte("x"), 100000), 7, true), candidates(e, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidate.Deflate.Level != 7 {
		t.Fatalf("%+v", res)
	}
}

func TestRunPicksEarliestOfEqualMatches(t *testing.T) {
	e := &fakeDeflate{}
	// Levels 3 and 3 again: both reproduce; index 2 must win even under parallelism.
	for i := 0; i < 20; i++ {
		res, err := Run(context.Background(), input(t, []byte("content"), 3, true), candidates(e, 0, 1, 3, 3, 3), 4)
		if err != nil {
			t.Fatal(err)
		}
		if res.Index != 2 {
			t.Fatalf("iteration %d: index %d", i, res.Index)
		}
	}
}

func TestRunNoMatch(t *testing.T) {
	e := &fakeDeflate{}
	_, err := Run(context.Background(), input(t, []byte("content"), 9, false), candidates(e, 0, 1, 2), 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
}

func TestRunSequentialWhenNotConcurrent(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, []byte("content"), 2, false), candidates(e, 0, 1, 2, 3), 8)
	if err != nil {
		t.Fatal(err)
	}
	// Sequential search stops at the first match, so level 3 is never tried.
	if res.Tried != 3 {
		t.Fatalf("tried %d", res.Tried)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &fakeDeflate{}
	_, err := Run(ctx, input(t, []byte("content"), 2, true), candidates(e, 0, 1, 2), 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRunTrailerMismatch(t *testing.T) {
	e := &fakeDeflate{}
	in := input(t, []byte("content"), 3, false)
	in.Trailer = []byte("WRONG!!")
	_, err := Run(context.Background(), in, candidates(e, 3), 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
}

func TestRunNoMatchIncludesNonMismatchError(t *testing.T) {
	e := &fakeDeflate{}
	be := &errDeflate{err: errors.New("boom")}
	ce := &cancelDeflate{}
	cands := []Candidate{
		{Engine: e, Deflate: &engine.DeflateParams{Level: 0}},
		{Engine: be, Deflate: &engine.DeflateParams{Level: 1}},
		{Engine: ce, Deflate: &engine.DeflateParams{Level: 2}},
		{Engine: e, Deflate: &engine.DeflateParams{Level: 3}},
	}
	// No candidate produces level 9's prefix, so every fakeDeflate candidate
	// mismatches; errDeflate and cancelDeflate always fail NewWriter.
	_, err := Run(context.Background(), input(t, []byte("content"), 9, false), cands, 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "boom") {
		t.Fatalf("expected error message to contain injected error, got %q", msg)
	}
	if strings.Contains(msg, ErrMismatch.Error()) {
		t.Fatalf("message should not include mismatch details: %q", msg)
	}
	if strings.Contains(msg, context.Canceled.Error()) {
		t.Fatalf("message should not include cancellation details: %q", msg)
	}
}
