package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrMismatch reports that a candidate's output differs from the reference.
var ErrMismatch = errors.New("search: output differs from reference")

// Compare is a writer that compares everything written to it against a
// reference reader and fails with ErrMismatch at the first difference. The
// search builds one per candidate; the confirming pass builds one over the
// whole input.
type Compare struct {
	ctx context.Context
	ref io.Reader
	buf []byte
	n   int64
}

// NewCompare returns a Compare over ref that also fails once ctx is done.
func NewCompare(ctx context.Context, ref io.Reader) *Compare {
	return &Compare{ctx: ctx, ref: ref}
}

// Matched is the number of bytes compared equal so far.
func (c *Compare) Matched() int64 { return c.n }

func (c *Compare) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if cap(c.buf) < len(p) {
		c.buf = make([]byte, len(p))
	}
	buf := c.buf[:len(p)]
	n, err := io.ReadFull(c.ref, buf)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("%w: reference ended at byte %d", ErrMismatch, c.n+int64(n))
		}
		return 0, err
	}
	if !bytes.Equal(p, buf) {
		off := 0
		for p[off] == buf[off] {
			off++
		}
		return 0, fmt.Errorf("%w: at byte %d", ErrMismatch, c.n+int64(off))
	}
	c.n += int64(len(p))
	return len(p), nil
}

// AtEOF returns nil when the reference has no bytes left.
func (c *Compare) AtEOF() error {
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
