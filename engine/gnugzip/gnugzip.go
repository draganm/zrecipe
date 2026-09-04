// Package gnugzip is a pure-Go port of GNU gzip's compressor, producing the
// raw deflate streams that the gzip program writes.
//
// The port (deflate.go, trees.go, bits.go) is derived from GNU gzip 1.14 and
// is licensed under the GNU General Public License version 3 or later; see
// COPYING in this directory. Its output is byte for byte what gzip 1.14
// produces when reading a regular file, which differs from zlib's at most
// levels because gzip decides where to end a deflate block with its own
// heuristic and never emits a stored file.
package gnugzip

import (
	"errors"
	"fmt"
	"io"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

// version is the GNU gzip release whose compressor this package ports. Bump
// it if a change to the port alters its output, since Params record it and
// Recompress refuses an engine whose version differs.
const version = "1.14"

// outFlush is how much pending output process accumulates before handing
// control back to the writer.
const outFlush = 64 << 10

// inChunk bounds how much of one Write is buffered ahead of the compressor.
const inChunk = 64 << 10

// Engine produces raw deflate streams the way GNU gzip does.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "gnu-gzip" }
func (*Engine) Version() string       { return version }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns two tiers: levels 1..9 ordered by the XFL hint, then
// the same levels with --rsyncable. GNU gzip has no level 0.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	var t1, t2 []engine.DeflateParams
	for _, l := range engine.LevelOrder(h.XFL) {
		if l == 0 {
			continue
		}
		t1 = append(t1, engine.DeflateParams{Level: l})
		t2 = append(t2, engine.DeflateParams{Level: l, Rsyncable: true})
	}
	return [][]engine.DeflateParams{t1, t2}
}

// NewWriter returns a writer that compresses into w. Close flushes.
func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Strategy != "" || p.WindowBits != 0 || p.MemLevel != 0 || p.BlockSize != 0 || p.Independent || p.SingleThread {
		return nil, errors.New("gnu-gzip: only level and rsyncable are supported")
	}
	if p.Level < 1 || p.Level > 9 {
		return nil, fmt.Errorf("gnu-gzip: level %d out of range 1..9", p.Level)
	}
	return &writer{d: newDeflater(p.Level, p.Rsyncable), w: w}, nil
}

// deflater is the whole state of one compression: the file-scope statics
// of deflate.c, trees.c and bits.c, plus the input queue.
type deflater struct {
	bitWriter
	treeState
	matchState

	level int
	rsync bool

	in    []byte // input not yet copied into the window
	inPos int
	inEOF bool
}

func newDeflater(level int, rsync bool) *deflater {
	d := &deflater{level: level, rsync: rsync}
	d.initTrees()
	d.lmInit(level)
	return d
}

// canRead reports whether a read of n bytes can be answered the way
// read(2) on a regular file answers it: with n bytes, or with whatever is
// left once the end of input is known.
func (d *deflater) canRead(n uint) bool {
	return uint(len(d.in)-d.inPos) >= n || d.inEOF
}

// readBuf is read_buf for a regular file: copy up to n bytes of input into
// window[off:] and return how many, 0 at end of input. Call canRead first.
func (d *deflater) readBuf(off, n uint) uint {
	if avail := uint(len(d.in) - d.inPos); n > avail {
		n = avail
	}
	copy(d.window[off:off+n], d.in[d.inPos:d.inPos+int(n)])
	d.inPos += int(n)
	return n
}

// feed queues p for the compressor, dropping input it has consumed.
func (d *deflater) feed(p []byte) {
	if d.inPos > 0 {
		n := copy(d.in, d.in[d.inPos:])
		d.in = d.in[:n]
		d.inPos = 0
	}
	d.in = append(d.in, p...)
}

type writer struct {
	d      *deflater
	w      io.Writer
	err    error
	closed bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.closed {
		return 0, errors.New("gnu-gzip: write after close")
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > inChunk {
			n = inChunk
		}
		w.d.feed(p[:n])
		if err := w.run(); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

// run advances the compressor as far as the queued input allows, writing
// its output through as it goes.
func (w *writer) run() error {
	for {
		r := w.d.process()
		if len(w.d.out) > 0 {
			_, err := w.w.Write(w.d.out)
			w.d.out = w.d.out[:0]
			if err != nil {
				w.err = err
				return err
			}
		}
		if r != outputFull {
			return nil
		}
	}
}

func (w *writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	w.d.inEOF = true
	return w.run()
}
