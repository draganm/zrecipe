// Package kpzstd is the klauspost/compress zstd engine.
package kpzstd

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
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

// Candidates returns one tier. Window size, checksum and content size come
// from the header. A window size that is not a power of two cannot come
// from klauspost, so no candidates are returned.
func (*Engine) Candidates(h *format.ZstdFrameHeader, _ int64) [][]engine.ZstdParams {
	if h.WindowLog > 0 && h.WindowSize != 1<<h.WindowLog {
		return nil
	}
	var tier []engine.ZstdParams
	for _, l := range levelOrder {
		p := engine.ZstdParams{
			Level:         l,
			WindowLog:     h.WindowLog,
			Checksum:      h.Checksum,
			ContentSize:   h.HasContentSize,
			PledgedSize:   h.HasContentSize,
			SingleSegment: h.SingleSegment,
		}
		tier = append(tier, p)
		if h.HasContentSize && !h.SingleSegment {
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
	return enc, nil
}

// allWriter buffers the input and encodes it in one EncodeAll call on Close.
type allWriter struct {
	w   io.Writer
	enc *zstd.Encoder
	buf []byte
}

func (a *allWriter) Write(p []byte) (int, error) {
	a.buf = append(a.buf, p...)
	return len(p), nil
}

func (a *allWriter) Close() error {
	defer a.enc.Close()
	_, err := a.w.Write(a.enc.EncodeAll(a.buf, nil))
	return err
}
