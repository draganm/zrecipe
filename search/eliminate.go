package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/draganm/zrecipe/engine"
)

const (
	// FirstWindow is the input prefix every candidate is fed before the
	// first verdicts are taken: two Feed writes.
	FirstWindow = 2 * engine.FeedSize
	// Margin is how far past its last competitor's death a lone survivor
	// must keep matching before the elimination settles on it.
	Margin = engine.FeedSize
	// MaxLockstep is how many tested candidates the lockstep carries between
	// rounds; once more than that many have survived a window the
	// elimination falls back to Run.
	MaxLockstep = 2
	// windowGrowth multiplies the window from one round to the next.
	windowGrowth = 4
)

// candidate is one contender's state across rounds. A candidate is tested
// once its engine has produced output and that output matched; until then
// the engine is buffering (pigz fills its first job, pgzip its first block)
// and nothing is known about it. cw compares the engine's output against a
// reader over the whole reference, read sequentially as output arrives;
// the engine writes cw (from a worker goroutine, for pigz) while the
// elimination reads its Matched and Err between windows, which cw
// synchronizes.
type candidate struct {
	idx      int
	w        io.WriteCloser
	cw       *Compare
	started  bool
	counted  bool  // started and already counted in tried
	pos      int64 // input fed so far
	tested   bool
	complete bool // fed to the end, closed, trailer matched: a full match
	dead     bool
	deathPos int64 // the input position a dead candidate had reached
	err      error // a non-mismatch failure that killed it
}

// elimination is one Eliminate call.
type elimination struct {
	ctx         context.Context
	in          *Input
	cands       []Candidate
	parallelism int
	size        int64
	alive       []*candidate // list order
	lastDeath   int64
	firstErr    error
	tried       int
}

// Eliminate finds the earliest candidate in list order that reproduces the
// input by advancing every candidate in lockstep windows over the spool
// and dropping each one at its first divergent byte. It settles on a lone
// survivor once that survivor has matched Margin bytes past the point where
// its last competitor died, without reading the rest of the spool, and
// reports that with Verified false; a candidate that reached the spool's
// end is Verified. When more than MaxLockstep tested candidates survive a
// window the elimination stops and Run, the sequential search, decides
// among the candidates still in play, so an input many candidates agree on
// for a long stretch costs what Run costs and no more. It returns
// ErrNoMatch when every candidate diverged.
func Eliminate(ctx context.Context, in *Input, cands []Candidate, parallelism int) (*Result, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: no candidates", ErrNoMatch)
	}
	// The lockstep keeps every candidate's reference reader open across
	// rounds. That needs independent readers, which payloadSource only
	// provides over an io.ReaderAt; a seek-only input has one shared
	// reader, so fall back to the sequential search, which runs each
	// candidate to completion one at a time.
	if !in.Concurrent {
		return Run(ctx, in, cands, 1)
	}
	if parallelism < 1 {
		parallelism = 1
	}
	e := &elimination{ctx: ctx, in: in, cands: cands, parallelism: parallelism, size: in.Spool.Size()}
	for i := range cands {
		e.alive = append(e.alive, &candidate{idx: i})
	}
	defer e.closeAll()
	window := min(e.size, int64(FirstWindow))
	for {
		fallback, err := e.round(window)
		if err != nil {
			return nil, err
		}
		if fallback {
			return e.fallback()
		}
		if len(e.alive) == 0 {
			return nil, e.noMatch()
		}
		if c := e.alive[0]; c.complete {
			// The window was the whole spool: every survivor is a full
			// match and the first in list order is the answer.
			return e.result(c), nil
		}
		tested := 0
		for _, c := range e.alive {
			if c.tested {
				tested++
			}
		}
		if len(e.alive) == 1 && tested == 1 {
			c := e.alive[0]
			if c.pos-e.lastDeath < Margin {
				if err := e.feed(c, min(e.size, roundUp(e.lastDeath+Margin, engine.FeedSize))); err != nil {
					return nil, err
				}
				if c.dead {
					e.retire()
					return nil, e.noMatch()
				}
			}
			return e.result(c), nil
		}
		// Grow the window fourfold each round. Two agreeing tested
		// candidates are told apart fastest by long windows; buffering
		// candidates (pigz, pgzip) that have not emitted keep the
		// elimination going until they do, at their block size, and a
		// larger step reaches that in fewer rounds. Overshooting the point
		// where a candidate emits costs nothing: a wrong candidate still
		// diverges within its first block of output, and the survivor is
		// fed no further than the last buffering candidate's block whatever
		// the step.
		window = min(e.size, window*windowGrowth)
	}
}

// round feeds every alive candidate to window on parallelism workers, then
// drops the dead. It reports fallback when more than MaxLockstep tested
// candidates survived the window: the hand-out stops at once, the
// candidates in flight finish, and the ones not reached stay alive, unfed,
// for Run to decide.
func (e *elimination) round(window int64) (fallback bool, err error) {
	var (
		mu       sync.Mutex
		next     int
		survived int
		stop     bool
		wg       sync.WaitGroup
	)
	for range e.parallelism {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if stop || next >= len(e.alive) {
					mu.Unlock()
					return
				}
				c := e.alive[next]
				next++
				mu.Unlock()

				ferr := e.feed(c, window)

				mu.Lock()
				switch {
				case ferr != nil:
					if err == nil {
						err = ferr
					}
					stop = true
				case !c.dead && c.tested:
					survived++
					if survived > MaxLockstep {
						stop = true
						fallback = true
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err != nil {
		return false, err
	}
	if cerr := e.ctx.Err(); cerr != nil {
		return false, cerr
	}
	e.retire()
	return fallback, nil
}

// feed hands c the spool from its position to target, starting its engine
// on first use and finishing it when target is the spool's end. It returns
// only errors that end the elimination (the context, the input); a
// candidate's own failure marks it dead. A candidate whose engine writes
// its output asynchronously (pigz) may not have produced the output for
// this window when feed returns; feed checks cw.Err after each window so a
// divergence that has surfaced kills the candidate, and Close at the end
// forces the rest out.
func (e *elimination) feed(c *candidate, target int64) error {
	if c.dead || c.complete {
		return nil
	}
	if !c.started {
		c.started = true
		// One reader over the whole reference, read sequentially as the
		// engine produces output. payloadSource gives each candidate its
		// own reader (a section reader over the io.ReaderAt input), so the
		// candidates run concurrently without sharing a position.
		ref, err := e.in.Payload(0)
		if err != nil {
			return err
		}
		c.cw = NewCompare(e.ctx, ref)
		w, err := newWriter(e.in, e.cands[c.idx], c.cw)
		if err != nil {
			return e.kill(c, err)
		}
		c.w = w
	}
	if n := target - c.pos; n > 0 {
		fed, err := engine.Feed(c.w, e.in.Spool.Section(c.pos, n))
		c.pos += fed
		if err != nil {
			return e.kill(c, err)
		}
	}
	// A candidate whose output has already diverged (surfaced by cw even
	// if the engine has not returned the error yet) is out.
	if cerr := c.cw.Err(); cerr != nil {
		return e.kill(c, cerr)
	}
	c.tested = c.cw.Matched() > 0
	if target < e.size {
		return nil
	}
	if err := c.w.Close(); err != nil {
		return e.kill(c, err)
	}
	if len(e.in.Trailer) > 0 {
		if _, err := c.cw.Write(e.in.Trailer); err != nil {
			return e.kill(c, err)
		}
	}
	if err := c.cw.AtEOF(); err != nil {
		return e.kill(c, err)
	}
	c.complete = true
	c.tested = true
	return nil
}

// kill records why c is out. A context error ends the elimination and is
// returned; a mismatch is the expected way out; anything else is
// remembered as the first non-mismatch error for ErrNoMatch's message. The
// engine writer is closed so that engines holding C memory release it;
// whatever it writes while closing is compared against the reference under
// cw's lock and ignored.
func (e *elimination) kill(c *candidate, err error) error {
	if cerr := e.ctx.Err(); cerr != nil {
		return cerr
	}
	c.dead = true
	c.deathPos = c.pos
	if !errors.Is(err, ErrMismatch) && !errors.Is(err, context.Canceled) {
		c.err = err
	}
	if c.w != nil {
		c.w.Close()
		c.w = nil
	}
	return nil
}

// retire drops the dead from alive, keeping list order, and folds their
// facts into the elimination: the furthest death and the first
// non-mismatch error.
func (e *elimination) retire() {
	kept := e.alive[:0]
	for _, c := range e.alive {
		if c.started && !c.counted {
			e.tried++
			c.counted = true
		}
		if !c.dead {
			kept = append(kept, c)
			continue
		}
		e.lastDeath = max(e.lastDeath, c.deathPos)
		if c.err != nil && e.firstErr == nil {
			e.firstErr = c.err
		}
	}
	for i := len(kept); i < len(e.alive); i++ {
		e.alive[i] = nil
	}
	e.alive = kept
}

// fallback hands the candidates still in play to Run, in list order, and
// maps its answer back. The lockstep states are released first.
func (e *elimination) fallback() (*Result, error) {
	idx := make([]int, 0, len(e.alive))
	sub := make([]Candidate, 0, len(e.alive))
	for _, c := range e.alive {
		idx = append(idx, c.idx)
		sub = append(sub, e.cands[c.idx])
	}
	e.closeAll()
	res, err := Run(e.ctx, e.in, sub, e.parallelism)
	if err != nil {
		return nil, err
	}
	res.Index = idx[res.Index]
	res.Tried += e.tried
	return res, nil
}

// result builds the Result for the settled candidate c.
func (e *elimination) result(c *candidate) *Result {
	if c.started && !c.counted {
		e.tried++
		c.counted = true
	}
	return &Result{Index: c.idx, Candidate: e.cands[c.idx], Tried: e.tried, Verified: c.complete}
}

// noMatch is the ErrNoMatch Run would report.
func (e *elimination) noMatch() error {
	if e.firstErr != nil {
		return fmt.Errorf("%w: tried %d candidates; first non-mismatch error: %v", ErrNoMatch, e.tried, e.firstErr)
	}
	return fmt.Errorf("%w: tried %d candidates", ErrNoMatch, e.tried)
}

// closeAll releases every engine still open.
func (e *elimination) closeAll() {
	for _, c := range e.alive {
		if c.w != nil {
			c.w.Close()
			c.w = nil
		}
	}
}

// roundUp rounds n up to a multiple of unit.
func roundUp(n, unit int64) int64 {
	return (n + unit - 1) / unit * unit
}
