// Package kpzstd is the klauspost/compress zstd engine.
package kpzstd

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

const (
	modulePath = "github.com/klauspost/compress"
	// maxEncodeAll bounds the memory used by single-segment and EncodeAll
	// candidates, which must hold the whole input.
	maxEncodeAll = 1 << 30
)

// Engine produces zstd frames with klauspost/compress/zstd.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "klauspost-zstd" }
func (*Engine) Version() string       { return engine.ModuleVersion(modulePath) }
func (*Engine) Format() engine.Format { return engine.FormatZstd }

// levelOrder lists klauspost levels: 2 default, 1 fastest, 3 better, 4 best.
var levelOrder = []int{2, 1, 3, 4}

// blockSize is the block klauspost's streaming writer cuts its input into
// at a level: 64 KiB at the fastest level, 128 KiB otherwise.
func blockSize(level int) int {
	if zstd.EncoderLevel(level) == zstd.SpeedFastest {
		return 64 << 10
	}
	return 128 << 10
}

// Candidates returns one tier. Window size, checksum and content size come
// from the header. A window size that is not a power of two cannot come
// from klauspost, so no candidates are returned. A first block flushed
// before it was full (format.ZstdFrameHeader.FlushedHead) can only come
// from the streaming writer with a Head of that size: the one-shot paths
// encode full blocks, so they are left out then.
func (*Engine) Candidates(h *format.ZstdFrameHeader, _ int64) [][]engine.ZstdParams {
	if h.WindowLog > 0 && h.WindowSize != 1<<h.WindowLog {
		return nil
	}
	var tier []engine.ZstdParams
	for _, l := range levelOrder {
		head, flushed := h.FlushedHead(blockSize(l))
		if flushed && h.SingleSegment {
			continue
		}
		p := engine.ZstdParams{
			Level:         l,
			WindowLog:     h.WindowLog,
			Checksum:      h.Checksum,
			ContentSize:   h.HasContentSize,
			PledgedSize:   h.HasContentSize,
			SingleSegment: h.SingleSegment,
			Head:          head,
		}
		tier = append(tier, p)
		if h.HasContentSize && !h.SingleSegment && !flushed {
			q := p
			q.EncodeAll = true
			tier = append(tier, q)
		}
	}
	return [][]engine.ZstdParams{tier}
}

func (*Engine) NewWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	if p.Level < 1 || p.Level > 4 {
		return nil, fmt.Errorf("klauspost-zstd: level %d out of range 1..4", p.Level)
	}
	if p.Long || p.Workers != 0 {
		return nil, errors.New("klauspost-zstd: long and workers are not supported")
	}
	if p.Head < 0 {
		return nil, fmt.Errorf("klauspost-zstd: head %d is negative", p.Head)
	}
	if p.Head > 0 && (p.SingleSegment || p.EncodeAll) {
		return nil, errors.New("klauspost-zstd: head needs the streaming path, not single_segment or encode_all")
	}
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.EncoderLevel(p.Level)),
		zstd.WithEncoderCRC(p.Checksum),
		zstd.WithEncoderConcurrency(1),
	}
	if p.WindowLog != 0 {
		opts = append(opts, zstd.WithWindowSize(1<<p.WindowLog))
	}
	if uncompressedSize == 0 {
		opts = append(opts, zstd.WithZeroFrames(true))
	}
	if p.SingleSegment || p.EncodeAll {
		if uncompressedSize > maxEncodeAll {
			return nil, fmt.Errorf("klauspost-zstd: input of %d bytes is too large for one-shot encoding", uncompressedSize)
		}
		opts = append(opts, zstd.WithSingleSegment(p.SingleSegment))
		enc, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			return nil, err
		}
		return &allWriter{w: w, enc: enc, buf: make([]byte, 0, uncompressedSize)}, nil
	}
	enc, err := zstd.NewWriter(w, opts...)
	if err != nil {
		return nil, err
	}
	if p.ContentSize {
		enc.ResetContentSize(w, uncompressedSize)
	}
	if p.Head > 0 {
		return &headWriter{enc: enc, left: p.Head}, nil
	}
	return enc, nil
}

// headWriter writes the first left bytes, flushes the encoder once, and
// streams the rest: what Encoder.ReadFrom does when it follows a Write.
// The split does not depend on how the input arrives, so the output is
// the same whatever the write shape.
type headWriter struct {
	enc  *zstd.Encoder
	left int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if h.left == 0 {
		return h.enc.Write(p)
	}
	n := min(h.left, len(p))
	if _, err := h.enc.Write(p[:n]); err != nil {
		return 0, err
	}
	h.left -= n
	if h.left == 0 {
		if err := h.enc.Flush(); err != nil {
			return n, err
		}
	}
	if n == len(p) {
		return n, nil
	}
	m, err := h.enc.Write(p[n:])
	return n + m, err
}

// Close ends the frame. Input shorter than the head is never flushed,
// which is also what ReadFrom would have done with nothing to read.
func (h *headWriter) Close() error { return h.enc.Close() }

// allWriter buffers the input and encodes it in one EncodeAll call on Close.
type allWriter struct {
	w      io.Writer
	enc    *zstd.Encoder
	buf    []byte
	closed bool
}

func (a *allWriter) Write(p []byte) (int, error) {
	a.buf = append(a.buf, p...)
	return len(p), nil
}

func (a *allWriter) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	defer a.enc.Close()
	_, err := a.w.Write(a.enc.EncodeAll(a.buf, nil))
	return err
}
