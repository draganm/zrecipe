package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// ErrNoMatch reports that no candidate reproduced the reference.
var ErrNoMatch = errors.New("search: no candidate reproduced the input")

// Candidate is one engine with one parameter set. Exactly one of Deflate or
// Zstd is set, matching the engine's format.
type Candidate struct {
	Engine  engine.Engine
	Deflate *engine.DeflateParams
	Zstd    *engine.ZstdParams
}

// Input describes the reference compressed stream and the spooled content.
type Input struct {
	Format format.Format
	// Payload returns a fresh reader over the compressed bytes that follow
	// the header, up to the end of the input.
	Payload func() (io.Reader, error)
	// Concurrent reports whether Payload may be called from several
	// goroutines at once. When false the search is sequential.
	Concurrent bool
	// Trailer is compared after the engine output; nil for zstd.
	Trailer          []byte
	Spool            *Spool
	UncompressedSize int64
}

// Result is a successful search.
type Result struct {
	Index     int
	Candidate Candidate
	Tried     int
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

				err := evaluate(cctx, in, cands[i])

				mu.Lock()
				delete(running, i)
				cancel()
				switch {
				case err == nil:
					if winner < 0 || i < winner {
						winner = i
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
	return &Result{Index: winner, Candidate: cands[winner], Tried: tried}, nil
}

// evaluate re-compresses the spool with one candidate and compares the
// output against the reference. It returns nil on an exact match.
func evaluate(ctx context.Context, in *Input, c Candidate) error {
	ref, err := in.Payload()
	if err != nil {
		return err
	}
	cw := newCompareWriter(ctx, ref)
	var w io.WriteCloser
	switch in.Format {
	case format.Gzip:
		e, ok := c.Engine.(engine.DeflateEngine)
		if !ok || c.Deflate == nil {
			return fmt.Errorf("search: %s is not a deflate candidate", c.Engine.Name())
		}
		w, err = e.NewWriter(cw, *c.Deflate)
	case format.Zstd:
		e, ok := c.Engine.(engine.ZstdEngine)
		if !ok || c.Zstd == nil {
			return fmt.Errorf("search: %s is not a zstd candidate", c.Engine.Name())
		}
		w, err = e.NewWriter(cw, *c.Zstd, in.UncompressedSize)
	default:
		return fmt.Errorf("search: unsupported format %q", in.Format)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, in.Spool.Reader()); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if len(in.Trailer) > 0 {
		if _, err := cw.Write(in.Trailer); err != nil {
			return err
		}
	}
	return cw.AtEOF()
}
