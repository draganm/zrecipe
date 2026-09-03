//go:build cgo

// Package libzstd is the cgo engine over the system libzstd.
package libzstd

/*
#cgo pkg-config: libzstd
#include <stdlib.h>
#include <string.h>
#include <zstd.h>
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

// Engine produces zstd frames with libzstd.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "libzstd" }
func (*Engine) Version() string       { return C.GoString(C.ZSTD_versionString()) }
func (*Engine) Format() engine.Format { return engine.FormatZstd }

var tier1Levels = []int{3, 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
var tier2Levels = []int{20, 21, 22, -1, -2, -3, -4, -5, -6, -7}

// Candidates returns two tiers. Header fields that are a direct function of
// the parameters (checksum, content size, single segment) are copied, not
// searched.
func (*Engine) Candidates(h *format.ZstdFrameHeader, _ int64) [][]engine.ZstdParams {
	base := engine.ZstdParams{Checksum: h.Checksum, ContentSize: h.HasContentSize, SingleSegment: h.SingleSegment}
	pledged := []bool{true}
	if !h.HasContentSize {
		pledged = []bool{false, true}
	}
	expand := func(levels []int, mutate func(*engine.ZstdParams)) []engine.ZstdParams {
		var out []engine.ZstdParams
		for _, l := range levels {
			for _, w := range []int{0, 1} {
				for _, pl := range pledged {
					p := base
					p.Level, p.Workers, p.PledgedSize = l, w, pl
					if w == 0 {
						p.EndWithData = !h.EmptyLastBlock
					}
					if mutate != nil {
						mutate(&p)
					}
					out = append(out, p)
				}
			}
		}
		return out
	}
	t1 := expand(tier1Levels, nil)
	t2 := expand(tier2Levels, nil)
	all := append(append([]int{}, tier1Levels...), 20, 21, 22)
	if h.WindowLog >= 27 || h.SingleSegment {
		wl := h.WindowLog
		if wl < 27 {
			wl = 27
		}
		t2 = append(t2, expand(all, func(p *engine.ZstdParams) { p.Long = true; p.WindowLog = wl })...)
	}
	if h.WindowLog > 0 {
		t2 = append(t2, expand(tier1Levels, func(p *engine.ZstdParams) { p.WindowLog = h.WindowLog })...)
	}
	return [][]engine.ZstdParams{t1, t2}
}

type writer struct {
	w      io.Writer
	cctx   *C.ZSTD_CCtx
	in     unsafe.Pointer
	inCap  int
	out    unsafe.Pointer
	outCap int
	err    error
	closed bool

	// endWithData holds back the most recently written chunk (already
	// copied into in, byte count in pending) instead of compressing it
	// with ZSTD_e_continue, so it can be handed to libzstd together with
	// ZSTD_e_end. This reproduces known-size producers such as the zstd
	// CLI reading a file, which never signals end with empty input.
	endWithData bool
	pending     int
}

func setParam(cctx *C.ZSTD_CCtx, param C.ZSTD_cParameter, v int) error {
	rc := C.ZSTD_CCtx_setParameter(cctx, param, C.int(v))
	if C.ZSTD_isError(rc) != 0 {
		return fmt.Errorf("libzstd: set parameter %d to %d: %s", int(param), v, C.GoString(C.ZSTD_getErrorName(rc)))
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (*Engine) NewWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	if p.EncodeAll {
		return nil, errors.New("libzstd: encode_all is not a libzstd parameter")
	}
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		return nil, errors.New("libzstd: ZSTD_createCCtx failed")
	}
	fail := func(err error) (io.WriteCloser, error) {
		C.ZSTD_freeCCtx(cctx)
		return nil, err
	}
	steps := []struct {
		param C.ZSTD_cParameter
		value int
		when  bool
	}{
		{C.ZSTD_c_compressionLevel, p.Level, true},
		{C.ZSTD_c_checksumFlag, boolInt(p.Checksum), true},
		{C.ZSTD_c_contentSizeFlag, boolInt(p.ContentSize), true},
		{C.ZSTD_c_windowLog, p.WindowLog, p.WindowLog != 0},
		{C.ZSTD_c_enableLongDistanceMatching, 1, p.Long},
		{C.ZSTD_c_nbWorkers, p.Workers, p.Workers > 0},
	}
	for _, s := range steps {
		if !s.when {
			continue
		}
		if err := setParam(cctx, s.param, s.value); err != nil {
			return fail(err)
		}
	}
	if p.PledgedSize {
		if rc := C.ZSTD_CCtx_setPledgedSrcSize(cctx, C.ulonglong(uncompressedSize)); C.ZSTD_isError(rc) != 0 {
			return fail(fmt.Errorf("libzstd: pledge size: %s", C.GoString(C.ZSTD_getErrorName(rc))))
		}
	}
	z := &writer{
		w:           w,
		cctx:        cctx,
		inCap:       int(C.ZSTD_CStreamInSize()),
		outCap:      int(C.ZSTD_CStreamOutSize()),
		endWithData: p.EndWithData && p.Workers == 0,
	}
	z.in = C.malloc(C.size_t(z.inCap))
	z.out = C.malloc(C.size_t(z.outCap))
	return z, nil
}

func (z *writer) emit(n C.size_t) error {
	if n == 0 {
		return nil
	}
	_, err := z.w.Write(unsafe.Slice((*byte)(z.out), int(n)))
	return err
}

// compressPending runs the ZSTD_e_continue loop over the first n bytes of
// z.in.
func (z *writer) compressPending(n int) error {
	in := C.ZSTD_inBuffer{src: z.in, size: C.size_t(n), pos: 0}
	for in.pos < in.size {
		out := C.ZSTD_outBuffer{dst: z.out, size: C.size_t(z.outCap), pos: 0}
		rc := C.ZSTD_compressStream2(z.cctx, &out, &in, C.ZSTD_e_continue)
		if C.ZSTD_isError(rc) != 0 {
			z.err = fmt.Errorf("libzstd: compress: %s", C.GoString(C.ZSTD_getErrorName(rc)))
			return z.err
		}
		if err := z.emit(out.pos); err != nil {
			z.err = err
			return err
		}
	}
	return nil
}

func (z *writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("libzstd: write after close")
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > z.inCap {
			n = z.inCap
		}
		if z.endWithData && z.pending > 0 {
			if err := z.compressPending(z.pending); err != nil {
				return total, err
			}
			z.pending = 0
		}
		C.memcpy(z.in, unsafe.Pointer(&p[0]), C.size_t(n))
		if z.endWithData {
			z.pending = n
		} else if err := z.compressPending(n); err != nil {
			return total, err
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
	size := 0
	if z.endWithData && z.pending > 0 {
		size = z.pending
	}
	in := C.ZSTD_inBuffer{src: z.in, size: C.size_t(size), pos: 0}
	var rc C.size_t = 1 // force the first iteration
	for in.pos < in.size || rc != 0 {
		out := C.ZSTD_outBuffer{dst: z.out, size: C.size_t(z.outCap), pos: 0}
		rc = C.ZSTD_compressStream2(z.cctx, &out, &in, C.ZSTD_e_end)
		if C.ZSTD_isError(rc) != 0 {
			z.err = fmt.Errorf("libzstd: finish: %s", C.GoString(C.ZSTD_getErrorName(rc)))
			return z.err
		}
		if err := z.emit(out.pos); err != nil {
			z.err = err
			return err
		}
	}
	return nil
}

func (z *writer) free() {
	C.ZSTD_freeCCtx(z.cctx)
	C.free(z.in)
	C.free(z.out)
}
