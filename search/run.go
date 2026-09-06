package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

// ErrNoMatch reports that no candidate reproduced the reference.
var ErrNoMatch = errors.New("search: no candidate reproduced the input")

// Candidate is one engine with one parameter set. Exactly one of Deflate or
// Zstd is set, matching the engine's format.
type Candidate struct {
	Engine  engine.Engine
	Deflate *engine.DeflateParams
	Zstd    *engine.ZstdParams
	// Buffered is how much input the engine consumes before it produces
	// any output, when the engine knows (libzstd's job-based path fills a
	// whole job first); zero when it streams from its first block or the
	// engine does not say. Eliminate does not start a candidate that could
	// not show output within UntestedLimit.
	Buffered int64
}

// Input describes the reference compressed stream and the spooled content.
type Input struct {
	Format format.Format
	// Payload returns a fresh reader over the compressed bytes that follow
	// the header, positioned off bytes into them and running to the end of
	// the input. The elimination re-positions a candidate's reference this
	// way at every window, so a shared seek-only reader works as well as an
	// io.ReaderAt.
	Payload func(off int64) (io.Reader, error)
	// Concurrent reports whether Payload may be called from several
	// goroutines at once. When false the search is sequential.
	Concurrent bool
	// Trailer is compared after the engine output; nil for zstd.
	Trailer          []byte
	Spool            *Spool
	UncompressedSize int64
	// VerifyLimit, when positive, is how many bytes of the reference a
	// candidate must reproduce to be accepted: it is then not fed further
	// and the Result is not Verified. Zero runs every candidate to the end.
	VerifyLimit int64
}

// Result is a successful search.
type Result struct {
	Index     int
	Candidate Candidate
	Tried     int
	// Verified reports that the candidate reproduced the whole input: true
	// from Run unless Input.VerifyLimit stopped the candidate first;
	// Eliminate settles most inputs on a prefix and leaves it false then.
	Verified bool
}

// Run evaluates candidates in order and returns the earliest one that
// reproduces the input. With parallelism above one and a concurrent input,
// candidates are evaluated by a worker pool; the result is still the
// earliest match in list order.
func Run(ctx context.Context, in *Input, cands []Candidate, parallelism int) (*Result, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: no candidates", ErrNoMatch)
	}
	if parallelism < 1 || !in.Concurrent {
		parallelism = 1
	}
	if parallelism > len(cands) {
		parallelism = len(cands)
	}

	var (
		mu       sync.Mutex
		next     int
		winner   = -1
		verified bool // the winner reproduced the whole input
		tried    int
		running  = map[int]context.CancelFunc{}
		firstErr error
		wg       sync.WaitGroup
	)
	for w := 0; w < parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if ctx.Err() != nil || next >= len(cands) || (winner >= 0 && next > winner) {
					mu.Unlock()
					return
				}
				i := next
				next++
				cctx, cancel := context.WithCancel(ctx)
				running[i] = cancel
				tried++
				mu.Unlock()

				whole, err := evaluate(cctx, in, cands[i])

				mu.Lock()
				delete(running, i)
				cancel()
				switch {
				case err == nil:
					if winner < 0 || i < winner {
						winner = i
						verified = whole
						for j, c := range running {
							if j > i {
								c()
							}
						}
					}
				case errors.Is(err, ErrMismatch), errors.Is(err, context.Canceled):
				default:
					if firstErr == nil {
						firstErr = err
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if winner < 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: tried %d candidates; first non-mismatch error: %v", ErrNoMatch, tried, firstErr)
		}
		return nil, fmt.Errorf("%w: tried %d candidates", ErrNoMatch, tried)
	}
	return &Result{Index: winner, Candidate: cands[winner], Tried: tried, Verified: verified}, nil
}

// evaluate re-compresses the spool with one candidate and compares the
// output against the reference. It returns a nil error on a match and
// reports whether the match covers the whole input, which it does not when
// in.VerifyLimit stopped the comparison first.
func evaluate(ctx context.Context, in *Input, c Candidate) (whole bool, err error) {
	ref, err := in.Payload(0)
	if err != nil {
		return false, err
	}
	cw := NewCompare(ctx, ref)
	cw.SetLimit(in.VerifyLimit)
	w, err := newWriter(in, c, cw)
	if err != nil {
		return false, err
	}
	// Feed, not io.Copy: an in-memory spool's reader has a WriterTo fast
	// path that would hand the engine everything in one Write, a shape
	// Recompress never uses.
	if _, err := engine.Feed(w, in.Spool.Reader()); err != nil {
		w.Close()
		return limitedOr(cw, err)
	}
	if err := w.Close(); err != nil {
		return limitedOr(cw, err)
	}
	if len(in.Trailer) > 0 {
		if _, err := cw.Write(in.Trailer); err != nil {
			return limitedOr(cw, err)
		}
	}
	if cw.Limited() {
		return false, nil
	}
	return true, cw.AtEOF()
}

// limitedOr maps a failure on the way through a candidate: once cw reached
// its limit the candidate is a match however the engine reported the stop
// (ErrLimit itself, or its own error for a writer that failed), not a
// whole one; otherwise err stands.
func limitedOr(cw *Compare, err error) (bool, error) {
	if cw.Limited() {
		return false, nil
	}
	return false, err
}

// newWriter opens c's engine writer over out for the input's format.
func newWriter(in *Input, c Candidate, out io.Writer) (io.WriteCloser, error) {
	switch in.Format {
	case format.Gzip:
		e, ok := c.Engine.(engine.DeflateEngine)
		if !ok || c.Deflate == nil {
			return nil, fmt.Errorf("search: %s is not a deflate candidate", c.Engine.Name())
		}
		return e.NewWriter(out, *c.Deflate)
	case format.Zstd:
		e, ok := c.Engine.(engine.ZstdEngine)
		if !ok || c.Zstd == nil {
			return nil, fmt.Errorf("search: %s is not a zstd candidate", c.Engine.Name())
		}
		return e.NewWriter(out, *c.Zstd, in.UncompressedSize)
	default:
		return nil, fmt.Errorf("search: unsupported format %q", in.Format)
	}
}
