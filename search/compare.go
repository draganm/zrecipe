package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrMismatch reports that a candidate's output differs from the reference.
var ErrMismatch = errors.New("search: output differs from reference")

// ErrLimit reports that a Compare reached its limit: everything written up
// to it compared equal and the comparison stopped there.
var ErrLimit = errors.New("search: verify limit reached")

// Compare is a writer that compares everything written to it against a
// reference reader and fails with ErrMismatch at the first difference. The
// search builds one per candidate; the confirming pass builds one over the
// whole input.
//
// It is safe for the writer and the reader of its state to be different
// goroutines: an engine whose output is produced asynchronously (pigz)
// writes it from a worker goroutine while the elimination reads Matched and
// Err between windows. Every field is guarded by mu; the reference is read
// only under mu, so it need not be safe for concurrent use itself.
type Compare struct {
	ctx context.Context

	mu      sync.Mutex
	ref     io.Reader
	buf     []byte
	n       int64
	err     error // the first mismatch (or reference read error), sticky
	done    bool  // Write has failed and will refuse further input
	limit   int64 // stop once this many bytes compared equal; 0 compares to the end
	limited bool  // the limit was reached; Write refuses further input
}

// NewCompare returns a Compare over ref that also fails once ctx is done.
// ref must run over the whole reference from the start; Compare reads it
// sequentially as output arrives and never repositions it.
func NewCompare(ctx context.Context, ref io.Reader) *Compare {
	return &Compare{ctx: ctx, ref: ref}
}

// SetLimit makes Write return ErrLimit once n bytes have compared equal
// and refuse further input. The write that reaches the limit is compared
// whole and counted. Zero, the default, compares to the end of the
// reference. Call it before the first Write.
func (c *Compare) SetLimit(n int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limit = n
}

// Limited reports that the limit was reached: at least that many bytes
// compared equal and the comparison stopped there. It stays false when
// there is no limit, and when a mismatch came first.
func (c *Compare) Limited() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limited
}

// Matched is the number of bytes compared equal so far.
func (c *Compare) Matched() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// Err is the first mismatch or reference error seen so far, or nil. It lets
// a caller learn that an asynchronously written candidate has already
// diverged without waiting for the engine to surface the error.
func (c *Compare) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Compare) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done {
		return 0, c.err
	}
	if c.limited {
		return 0, ErrLimit
	}
	if cap(c.buf) < len(p) {
		c.buf = make([]byte, len(p))
	}
	buf := c.buf[:len(p)]
	rn, err := io.ReadFull(c.ref, buf)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			c.fail(fmt.Errorf("%w: reference ended at byte %d", ErrMismatch, c.n+int64(rn)))
			return 0, c.err
		}
		c.fail(err)
		return 0, c.err
	}
	if !bytes.Equal(p, buf) {
		off := 0
		for p[off] == buf[off] {
			off++
		}
		c.fail(fmt.Errorf("%w: at byte %d", ErrMismatch, c.n+int64(off)))
		return 0, c.err
	}
	c.n += int64(len(p))
	if c.limit > 0 && c.n >= c.limit {
		c.limited = true
		return len(p), ErrLimit
	}
	return len(p), nil
}

// fail records the first error and latches done. The caller holds mu.
func (c *Compare) fail(err error) {
	if c.err == nil {
		c.err = err
	}
	c.done = true
}

// AtEOF returns nil when the reference has no bytes left.
func (c *Compare) AtEOF() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	var b [1]byte
	_, err := io.ReadFull(c.ref, b[:])
	switch {
	case err == nil:
		return fmt.Errorf("%w: reference has extra bytes after %d", ErrMismatch, c.n)
	case errors.Is(err, io.EOF):
		return nil
	default:
		return err
	}
}
