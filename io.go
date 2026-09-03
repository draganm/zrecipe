package compprysm

import (
	"context"
	"encoding/hex"
	"io"

	"lukechampine.com/blake3"
)

func newHasher() *blake3.Hasher { return blake3.New(32, nil) }

func digestOf(h *blake3.Hasher, n int64) Digest {
	return Digest{Blake3: hex.EncodeToString(h.Sum(nil)), Size: n}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ctxWriter fails writes once ctx is done, so engines that do not take a
// context still stop within one write.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
