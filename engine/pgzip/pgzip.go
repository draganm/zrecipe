// Package pgzip reproduces the deflate streams that klauspost/pgzip, the
// parallel gzip used by umoci (and through it by rockcraft and every
// Canonical rock on Docker Hub), writes. pgzip cuts the input into blocks of
// 1 MiB, compresses each block with klauspost/compress/flate primed with the
// last 16 KiB of the block before it, sync-flushes after every block, and
// closes the stream after the last block, which it compresses even when it
// is empty. The bytes depend on the klauspost/compress generation as much
// as on that scheme: this engine drives a copy of klauspost/compress/flate
// v1.11.3 (the flate directory below this package), the generation umoci
// pins, because the module version the rest of zrecipe links compresses
// differently and Go cannot load two versions of one module.
package pgzip

import (
	"errors"
	"fmt"
	"io"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/pgzip/flate"
	"github.com/draganm/zrecipe/format"
)

const (
	// pgzipVersion is the klauspost/pgzip release whose writer this package
	// ports, and flateVersion the klauspost/compress release whose flate
	// package is copied below it. Bump flateVersion if that copy changes in
	// a way that alters its output: Params record the combined version and
	// Recompress refuses an engine whose version differs.
	pgzipVersion = "1.2.6"
	flateVersion = "1.11.3"

	tailSize     = 16384            // pgzip's tailSize: the dictionary carried into the next block
	defaultBlock = 1024             // pgzip's defaultBlockSize, in KiB
	minBlock     = tailSize>>10 + 1 // SetConcurrency rejects blocks of tailSize bytes or fewer
)

// otherBlocks are the block sizes, in KiB, tried after the default.
var otherBlocks = []int{128, 256, 512, 2048, 4096}

// Engine produces raw deflate streams the way klauspost/pgzip does.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "pgzip" }
func (*Engine) Version() string       { return pgzipVersion + "+klauspost-compress" + flateVersion }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// levelOrder is engine.LevelOrder except that, when XFL carries no hint,
// klauspost's default level 5 comes first: pgzip callers mostly take the
// default. HuffmanOnly is last.
func levelOrder(xfl byte) []int {
	var levels []int
	if xfl == 2 || xfl == 4 {
		levels = engine.LevelOrder(xfl)
	} else {
		levels = []int{5, 6, 7, 4, 8, 3, 9, 2, 1, 0}
	}
	return append(levels, flate.HuffmanOnly)
}

// Candidates returns two tiers: the default block at every level, then the
// other block sizes that can change the output for this input size. A block
// larger than the input yields one block and the same bytes as every other
// such size, so those are tried once at most, and not at all when the
// default already covers them. A block exactly the size of the input is
// not one of them: it fills, and Close then compresses an empty block
// after it.
func (*Engine) Candidates(h *format.GzipHeader, size int64) [][]engine.DeflateParams {
	levels := levelOrder(h.XFL)
	var t1, t2 []engine.DeflateParams
	for _, l := range levels {
		t1 = append(t1, engine.DeflateParams{Level: l})
	}
	needOne := size >= defaultBlock<<10
	for _, b := range otherBlocks {
		if int64(b)<<10 > size {
			if !needOne {
				continue
			}
			needOne = false
		}
		for _, l := range levels {
			t2 = append(t2, engine.DeflateParams{Level: l, BlockSize: b})
		}
	}
	return [][]engine.DeflateParams{t1, t2}
}

// NewWriter returns a writer that compresses into w. Close flushes.
func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Level == flate.DefaultCompression || p.Level < flate.HuffmanOnly || p.Level > flate.BestCompression {
		return nil, fmt.Errorf("pgzip: level %d out of range -2, 0..9 (klauspost's default, -1, is level 5)", p.Level)
	}
	if p.Strategy != "" || p.WindowBits != 0 || p.MemLevel != 0 || p.Rsyncable || p.Independent || p.SingleThread {
		return nil, errors.New("pgzip: only level and block_size are supported")
	}
	block := p.BlockSize
	if block == 0 {
		block = defaultBlock
	}
	if block < minBlock {
		return nil, fmt.Errorf("pgzip: block_size %d KiB is not above the %d KiB tail", block, tailSize>>10)
	}
	fw, err := flate.NewWriter(w, p.Level)
	if err != nil {
		return nil, fmt.Errorf("pgzip: %v", err)
	}
	return &writer{fw: fw, w: w, block: block << 10}, nil
}

// writer is pgzip's Writer with the goroutines taken out: blocks are
// compressed in order as they fill, and Close compresses whatever is left.
type writer struct {
	fw    *flate.Writer // reused across blocks through ResetDict, as pgzip's pool does
	w     io.Writer
	block int    // bytes
	buf   []byte // the block being filled
	tail  []byte // last tailSize bytes of the previous block; nil after a shorter one

	err    error
	closed bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("pgzip: write after close")
	}
	if w.err != nil {
		return 0, w.err
	}
	var n int
	for len(p) > 0 {
		k := min(len(p), w.block-len(w.buf))
		w.buf = append(w.buf, p[:k]...)
		p = p[k:]
		n += k
		if len(w.buf) == w.block {
			if w.err = w.compress(false); w.err != nil {
				return n, w.err
			}
		}
	}
	return n, nil
}

func (w *writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	if w.err != nil {
		return w.err
	}
	w.err = w.compress(true)
	return w.err
}

// compress is pgzip's compressBlock: the current block is compressed as a
// fresh deflate stream primed with the previous tail, sync-flushed, and
// closed if it is the last.
func (w *writer) compress(last bool) error {
	w.fw.ResetDict(w.w, w.tail)
	if _, err := w.fw.Write(w.buf); err != nil {
		return err
	}
	if err := w.fw.Flush(); err != nil {
		return err
	}
	if last {
		if err := w.fw.Close(); err != nil {
			return err
		}
	}
	if len(w.buf) > tailSize {
		w.tail = append(w.tail[:0], w.buf[len(w.buf)-tailSize:]...)
	} else {
		w.tail = nil
	}
	w.buf = w.buf[:0]
	return nil
}
