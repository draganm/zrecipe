//go:build cgo

// Package zlib is the cgo engine over the system zlib, producing raw
// deflate streams.
package zlib

/*
#cgo pkg-config: zlib
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

// deflateInit2 is a macro; wrap it so cgo can call it.
static int cp_deflate_init(z_streamp s, int level, int windowBits, int memLevel, int strategy) {
	return deflateInit2(s, level, Z_DEFLATED, windowBits, memLevel, strategy);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// bufSize must exceed 65535 (deflate's max stored-block length) plus the
// 5-byte stored-block header by enough margin that a level-0 write is never
// output-buffer-limited to a shorter block: with avail_out at exactly
// 64 KiB, zlib caps each stored block at avail_out-5 (65531) instead of the
// canonical 65535.
const bufSize = 128 << 10

var strategies = map[string]C.int{
	engine.StrategyDefault:     C.Z_DEFAULT_STRATEGY,
	engine.StrategyFiltered:    C.Z_FILTERED,
	engine.StrategyHuffmanOnly: C.Z_HUFFMAN_ONLY,
	engine.StrategyRLE:         C.Z_RLE,
	engine.StrategyFixed:       C.Z_FIXED,
}

var searchStrategies = []string{engine.StrategyFiltered, engine.StrategyHuffmanOnly, engine.StrategyRLE, engine.StrategyFixed}

// Engine produces raw deflate streams with zlib.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "zlib" }
func (*Engine) Version() string       { return C.GoString(C.zlibVersion()) }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns three tiers: default strategy at levels 0..9; other
// strategies at levels 1..9; then memory level and window bits variations.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	levels := engine.LevelOrder(h.XFL)
	var t1, t2, t3 []engine.DeflateParams
	for _, l := range levels {
		t1 = append(t1, engine.DeflateParams{Level: l, Strategy: engine.StrategyDefault, WindowBits: 15, MemLevel: 8})
	}
	for _, s := range searchStrategies {
		for _, l := range levels {
			if l == 0 {
				continue // stored blocks ignore the strategy
			}
			t2 = append(t2, engine.DeflateParams{Level: l, Strategy: s, WindowBits: 15, MemLevel: 8})
		}
	}
	for _, l := range levels {
		if l == 0 {
			continue
		}
		for ml := 9; ml >= 1; ml-- {
			for wb := 15; wb >= 9; wb-- {
				if ml == 8 && wb == 15 {
					continue // already in tier 1
				}
				t3 = append(t3, engine.DeflateParams{Level: l, Strategy: engine.StrategyDefault, WindowBits: wb, MemLevel: ml})
			}
		}
	}
	return [][]engine.DeflateParams{t1, t2, t3}
}

type writer struct {
	w      io.Writer
	strm   *C.z_stream
	in     unsafe.Pointer
	out    unsafe.Pointer
	err    error
	closed bool
}

func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	strategy := p.Strategy
	if strategy == "" {
		strategy = engine.StrategyDefault
	}
	st, ok := strategies[strategy]
	if !ok {
		return nil, fmt.Errorf("zlib: unknown strategy %q", p.Strategy)
	}
	wb, ml := p.WindowBits, p.MemLevel
	if wb == 0 {
		wb = 15
	}
	if ml == 0 {
		ml = 8
	}
	if p.Level < 0 || p.Level > 9 {
		return nil, fmt.Errorf("zlib: level %d out of range 0..9", p.Level)
	}
	if wb < 9 || wb > 15 {
		return nil, fmt.Errorf("zlib: window_bits %d out of range 9..15", wb)
	}
	if ml < 1 || ml > 9 {
		return nil, fmt.Errorf("zlib: mem_level %d out of range 1..9", ml)
	}
	strm := (*C.z_stream)(C.calloc(1, C.size_t(unsafe.Sizeof(C.z_stream{}))))
	if strm == nil {
		return nil, errors.New("zlib: out of memory")
	}
	// Negative window bits selects raw deflate without the zlib wrapper.
	if rc := C.cp_deflate_init(strm, C.int(p.Level), C.int(-wb), C.int(ml), st); rc != C.Z_OK {
		C.free(unsafe.Pointer(strm))
		return nil, fmt.Errorf("zlib: deflateInit2 returned %d", rc)
	}
	z := &writer{w: w, strm: strm, in: C.malloc(bufSize), out: C.malloc(bufSize)}
	z.resetOut()
	return z, nil
}

func (z *writer) resetOut() {
	z.strm.next_out = (*C.Bytef)(z.out)
	z.strm.avail_out = bufSize
}

// flushOut writes whatever deflate produced and resets the output buffer.
func (z *writer) flushOut() error {
	produced := bufSize - int(z.strm.avail_out)
	if produced > 0 {
		if _, err := z.w.Write(unsafe.Slice((*byte)(z.out), produced)); err != nil {
			return err
		}
	}
	z.resetOut()
	return nil
}

func (z *writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("zlib: write after close")
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > bufSize {
			n = bufSize
		}
		C.memcpy(z.in, unsafe.Pointer(&p[0]), C.size_t(n))
		z.strm.next_in = (*C.Bytef)(z.in)
		z.strm.avail_in = C.uInt(n)
		for z.strm.avail_in > 0 {
			rc := C.deflate(z.strm, C.Z_NO_FLUSH)
			if rc != C.Z_OK && rc != C.Z_BUF_ERROR {
				z.err = fmt.Errorf("zlib: deflate returned %d", rc)
				return total, z.err
			}
			if err := z.flushOut(); err != nil {
				z.err = err
				return total, err
			}
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (z *writer) Close() error {
	if z.closed {
		return z.err
	}
	z.closed = true
	defer z.free()
	if z.err != nil {
		return z.err
	}
	z.strm.avail_in = 0
	for {
		rc := C.deflate(z.strm, C.Z_FINISH)
		if err := z.flushOut(); err != nil {
			z.err = err
			return err
		}
		if rc == C.Z_STREAM_END {
			return nil
		}
		if rc != C.Z_OK && rc != C.Z_BUF_ERROR {
			z.err = fmt.Errorf("zlib: deflate(Z_FINISH) returned %d", rc)
			return z.err
		}
	}
}

func (z *writer) free() {
	C.deflateEnd(z.strm)
	C.free(z.in)
	C.free(z.out)
	C.free(unsafe.Pointer(z.strm))
}
