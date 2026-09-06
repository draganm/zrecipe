//go:build cgo

// Package libzstd is the cgo engine over the system libzstd.
package libzstd

/*
#cgo pkg-config: libzstd
#define ZSTD_STATIC_LINKING_ONLY
#define ZSTD_DISABLE_DEPRECATE_WARNINGS
#include <stdlib.h>
#include <zstd.h>
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

// Engine produces zstd frames with libzstd.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "libzstd" }
func (*Engine) Version() string       { return C.GoString(C.ZSTD_versionString()) }
func (*Engine) Format() engine.Format { return engine.FormatZstd }

// blockSizeMax is the block libzstd cuts its input into (ZSTD_BLOCKSIZE_MAX).
const blockSizeMax = 128 << 10

// ldmDefaultWindowLog is the window long distance matching switches to
// (ZSTD_LDM_DEFAULT_WINDOW_LOG, internal to libzstd).
const ldmDefaultWindowLog = 27

var tier1Levels = []int{3, 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
var tier2Levels = []int{20, 21, 22, -1, -2, -3, -4, -5, -6, -7}

// Candidates returns two tiers. Header fields that are a direct function of
// the parameters (checksum, content size, single segment) are copied, not
// searched. The first tier is the single-thread path at the levels whose
// own window is the header's; the second adds the job-based path (Workers
// 1: libzstd's multithreaded compressor, which the zstd CLI uses even with
// one thread), the ultra and negative levels, long distance matching for
// wide windows, and the other levels with the header's window set
// explicitly.
//
// A first block flushed before it was full
// (format.ZstdFrameHeader.FlushedHead) rules libzstd out: its streaming
// paths cut full blocks until the end of the input, so no candidate is
// returned rather than every one dying at that block. For the same reason
// a candidate is offered only if it writes the header's window descriptor
// (see resolve): libzstd derives that byte from the parameters alone, so
// any other candidate would die at byte six, and the job-based ones
// libzstd's own compressor drives only after a whole first job.
func (*Engine) Candidates(h *format.ZstdFrameHeader, uncompressedSize int64) [][]engine.ZstdParams {
	if _, flushed := h.FlushedHead(blockSizeMax); flushed {
		return nil
	}
	if !h.SingleSegment && h.WindowSize != 0 && h.WindowSize != 1<<h.WindowLog {
		return nil
	}
	base := engine.ZstdParams{Checksum: h.Checksum, ContentSize: h.HasContentSize, SingleSegment: h.SingleSegment}
	pledged := []bool{true}
	if !h.HasContentSize {
		pledged = []bool{false, true}
	}
	// fits reports whether p writes the header's window descriptor: the
	// window it resolves to, or none at all when the pledged size fits in
	// it, which makes libzstd write a single-segment frame.
	fits := func(p engine.ZstdParams) bool {
		cp := resolve(p, uncompressedSize)
		single := p.PledgedSize && p.ContentSize && uint64(1)<<cp.windowLog >= uint64(uncompressedSize)
		if h.SingleSegment {
			return single
		}
		return !single && int(cp.windowLog) == h.WindowLog
	}
	expand := func(levels []int, workers []int, mutate func(*engine.ZstdParams)) []engine.ZstdParams {
		var out []engine.ZstdParams
		for _, l := range levels {
			for _, w := range workers {
				for _, pl := range pledged {
					p := base
					p.Level, p.Workers, p.PledgedSize = l, w, pl
					if w == 0 {
						p.EndWithData = !h.EmptyLastBlock
					}
					if mutate != nil {
						mutate(&p)
					}
					if fits(p) {
						out = append(out, p)
					}
				}
			}
		}
		return out
	}
	single, both := []int{0}, []int{0, 1}
	t1 := expand(tier1Levels, single, nil)
	t2 := expand(tier1Levels, []int{1}, nil)
	t2 = append(t2, expand(tier2Levels, both, nil)...)
	all := append(append([]int{}, tier1Levels...), 20, 21, 22)
	if h.WindowLog >= 27 || h.SingleSegment {
		wl := h.WindowLog
		if wl < 27 {
			wl = 27
		}
		t2 = append(t2, expand(all, both, func(p *engine.ZstdParams) { p.Long = true; p.WindowLog = wl })...)
	}
	if h.WindowLog > 0 {
		var levels []int
		for _, l := range tier1Levels {
			p := base
			p.Level = l
			if int(resolve(p, uncompressedSize).windowLog) != h.WindowLog {
				levels = append(levels, l)
			}
		}
		t2 = append(t2, expand(levels, both, func(p *engine.ZstdParams) { p.WindowLog = h.WindowLog })...)
	}
	if len(t1) == 0 && len(t2) == 0 {
		return nil
	}
	return [][]engine.ZstdParams{t1, t2}
}

// resolve returns the compression parameters libzstd settles on for p over
// an input of uncompressedSize bytes: the level's table row for the
// pledged size (or for an unknown size), long distance matching's window,
// the explicit window if any, adjusted for the pledged size the way
// ZSTD_getCParamsFromCCtxParams does when the stream starts.
func resolve(p engine.ZstdParams, uncompressedSize int64) C.ZSTD_compressionParameters {
	size := C.ulonglong(C.ZSTD_CONTENTSIZE_UNKNOWN)
	if p.PledgedSize {
		size = C.ulonglong(uncompressedSize)
	}
	cp := C.ZSTD_getCParams(C.int(p.Level), size, 0)
	override := p.WindowLog != 0
	if p.Long {
		cp.windowLog = ldmDefaultWindowLog
		override = true
	}
	if p.WindowLog != 0 {
		cp.windowLog = C.uint(p.WindowLog)
	}
	if override {
		cp = C.ZSTD_adjustCParams(cp, size, 0)
	}
	return cp
}

// Buffered reports the input a job-based candidate (Workers > 0) takes in
// before any output appears: nothing when jobWriter can compress its first
// job chunk by chunk, otherwise the whole first job, which libzstd's own
// compressor fills before it starts.
func (*Engine) Buffered(p engine.ZstdParams, uncompressedSize int64) int64 {
	if p.Workers == 0 || emulated(p, uncompressedSize) {
		return 0
	}
	return jobSize(p, uncompressedSize)
}

// jobSize is the first job of the job-based path, following
// ZSTDMT_computeTargetJobLog: 2^max(20, windowLog+2) bytes, or with long
// distance matching 2^max(21, cycleLog+3) with cycleLog the chain log less
// one for the binary-tree strategies, capped at 2^30, over the parameters
// resolve settles on.
func jobSize(p engine.ZstdParams, uncompressedSize int64) int64 {
	cp := resolve(p, uncompressedSize)
	var jobLog uint
	if p.Long {
		cycleLog := uint(cp.chainLog)
		if cp.strategy >= C.ZSTD_btlazy2 {
			cycleLog--
		}
		jobLog = max(21, cycleLog+3)
	} else {
		jobLog = max(20, uint(cp.windowLog)+2)
	}
	return 1 << min(jobLog, 30)
}

const (
	// jobChunk is the unit ZSTDMT compresses a job in: four blocks.
	jobChunk = 4 * blockSizeMax
	// jobSizeMin is ZSTDMT_JOBSIZE_MIN: a known input size up to this
	// makes libzstd compress on its single-thread path whatever the
	// worker count.
	jobSizeMin = 512 << 10
	// maxEmulatedJob bounds the first job jobWriter keeps in memory for
	// the hand-over to libzstd's compressor; larger jobs (the ultra
	// levels, wide windows) go straight to that compressor and report
	// their job through Buffered.
	maxEmulatedJob = 32 << 20
)

// emulated reports whether the job-based path for p is driven by
// jobWriter: an input libzstd would compress in jobs of at most
// maxEmulatedJob without long distance matching, whose first job the
// buffer-less API reproduces. libzstd takes the single-thread path for a
// known size up to jobSizeMin, and ends an empty unknown-size input
// differently, so those go to its own compressor too.
func emulated(p engine.ZstdParams, uncompressedSize int64) bool {
	if p.Workers == 0 || p.Long || uncompressedSize == 0 {
		return false
	}
	if p.PledgedSize && uncompressedSize <= jobSizeMin {
		return false
	}
	return jobSize(p, uncompressedSize) <= maxEmulatedJob
}

type writer struct {
	w      io.Writer
	cctx   *C.ZSTD_CCtx
	in     unsafe.Pointer
	inBuf  []byte // the C input buffer as a Go slice
	inCap  int
	out    unsafe.Pointer
	outCap int
	err    error
	closed bool

	// Input is batched in in (pending bytes so far) and handed to libzstd
	// in ZSTD_CStreamInSize batches, the read size of the zstd CLI. A
	// batch is compressed with ZSTD_e_continue only once more input
	// arrives after it, so the last one is still pending at Close. The
	// stream therefore depends only on the content, never on how the
	// caller split it across Writes: what is handed to libzstd together
	// with ZSTD_e_end, and whether the first call is that one (which makes
	// libzstd pledge the size itself and tune its parameters to it), would
	// otherwise follow the size of the last Write (issue #1).
	pending int
	// endWithData hands the last batch to libzstd together with
	// ZSTD_e_end instead of compressing it with ZSTD_e_continue first.
	// This reproduces known-size producers such as the zstd CLI reading a
	// file, which never signals end with empty input.
	endWithData bool
	// jobs marks the job-based path (Workers > 0), whose output is
	// produced by a worker thread; see compressPending.
	jobs bool
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

// NewWriter returns a writer that compresses into w with p. The
// single-thread path (Workers 0) and the job-based path for inputs
// emulated does not cover drive libzstd's streaming compressor directly;
// the other job-based candidates go through jobWriter, which shows output
// chunk by chunk.
func (*Engine) NewWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	if p.EncodeAll {
		return nil, errors.New("libzstd: encode_all is not a libzstd parameter")
	}
	if emulated(p, uncompressedSize) {
		return newJobWriter(w, p, uncompressedSize)
	}
	return newDirectWriter(w, p, uncompressedSize)
}

// newDirectWriter drives libzstd's streaming compressor: the single-thread
// path, or with Workers > 0 its own job-based one, which shows nothing
// until its first job is full.
func newDirectWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
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
		jobs:        p.Workers > 0,
	}
	z.in = C.malloc(C.size_t(z.inCap))
	z.inBuf = unsafe.Slice((*byte)(z.in), z.inCap)
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
// z.in. On the job-based path it then calls once more with no input,
// which makes libzstd wait for the running job to have produced something
// and hand it over: the output then follows the input at the pace of the
// worker instead of surfacing whenever the job happens to get ahead of
// the caller, so a candidate's first output shows within a batch of its
// first job. The bytes are the same either way.
func (z *writer) compressPending(n int) error {
	in := C.ZSTD_inBuffer{src: z.in, size: C.size_t(n), pos: 0}
	for in.pos < in.size {
		if err := z.compressStep(&in); err != nil {
			return err
		}
	}
	if z.jobs {
		if err := z.compressStep(&in); err != nil {
			return err
		}
	}
	return nil
}

// compressStep is one ZSTD_compressStream2 call with ZSTD_e_continue over
// what remains of in, emitting whatever it produced.
func (z *writer) compressStep(in *C.ZSTD_inBuffer) error {
	out := C.ZSTD_outBuffer{dst: z.out, size: C.size_t(z.outCap), pos: 0}
	rc := C.ZSTD_compressStream2(z.cctx, &out, in, C.ZSTD_e_continue)
	if C.ZSTD_isError(rc) != 0 {
		z.err = fmt.Errorf("libzstd: compress: %s", C.GoString(C.ZSTD_getErrorName(rc)))
		return z.err
	}
	if err := z.emit(out.pos); err != nil {
		z.err = err
		return err
	}
	return nil
}

// Write buffers p and compresses each full batch once input follows it.
func (z *writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("libzstd: write after close")
	}
	total := 0
	for len(p) > 0 {
		if z.pending == z.inCap {
			if err := z.compressPending(z.pending); err != nil {
				return total, err
			}
			z.pending = 0
		}
		n := copy(z.inBuf[z.pending:], p)
		z.pending += n
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
	if z.endWithData {
		size = z.pending
	} else if z.pending > 0 {
		if err := z.compressPending(z.pending); err != nil {
			return err
		}
	}
	z.pending = 0
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

// jobWriter reproduces libzstd's job-based path while showing output as
// soon as libzstd would compress it rather than once a whole job is full,
// so that the search can drop a wrong candidate after one chunk instead of
// after a job at that level (which stopping libzstd's own compressor
// still costs, since freeing it waits for the job in flight). The first
// job is compressed with the buffer-less API exactly as
// ZSTDMT_compressionJob does: begin with the resolved parameters and the
// frame's pledged size, continue chunk by chunk, and end the frame if the
// input ends within the job. If the input outlasts the job, the rest is
// handed to libzstd's own job-based compressor: the first job is replayed
// into it and the bytes it writes for that job are checked against the
// ones already written before its output is passed on.
type jobWriter struct {
	w       io.Writer
	p       engine.ZstdParams
	size    int64
	jobSize int
	cctx    *C.ZSTD_CCtx
	out     unsafe.Pointer
	outCap  int
	// job is the first job's input so far, in a C buffer of the job's
	// size: the buffer-less API keeps pointers into the input it has
	// compressed, so the input must stay put while the job goes on.
	jobBuf   unsafe.Pointer
	job      []byte
	chunked  int    // bytes of job already compressed
	emitted  []byte // output written for the first job so far
	direct   io.WriteCloser
	replayed *replayCheck
	err      error
	closed   bool
}

var errReplay = errors.New("libzstd: job-based compressor did not reproduce the first job written chunk by chunk")

func newJobWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		return nil, errors.New("libzstd: ZSTD_createCCtx failed")
	}
	pledged := C.ulonglong(C.ZSTD_CONTENTSIZE_UNKNOWN)
	if p.PledgedSize {
		pledged = C.ulonglong(uncompressedSize)
	}
	var params C.ZSTD_parameters
	params.cParams = resolve(p, uncompressedSize)
	params.fParams.contentSizeFlag = C.int(boolInt(p.ContentSize))
	params.fParams.checksumFlag = C.int(boolInt(p.Checksum))
	if rc := C.ZSTD_compressBegin_advanced(cctx, nil, 0, params, pledged); C.ZSTD_isError(rc) != 0 {
		C.ZSTD_freeCCtx(cctx)
		return nil, fmt.Errorf("libzstd: begin: %s", C.GoString(C.ZSTD_getErrorName(rc)))
	}
	j := &jobWriter{
		w:       w,
		p:       p,
		size:    uncompressedSize,
		jobSize: int(jobSize(p, uncompressedSize)),
		cctx:    cctx,
		outCap:  int(C.ZSTD_compressBound(jobChunk)) + 64,
	}
	j.out = C.malloc(C.size_t(j.outCap))
	j.jobBuf = C.malloc(C.size_t(j.jobSize))
	j.job = unsafe.Slice((*byte)(j.jobBuf), j.jobSize)[:0]
	return j, nil
}

// compressChunk compresses job[j.chunked:j.chunked+n] with
// ZSTD_compressContinue, or ZSTD_compressEnd when it ends the frame, and
// writes the output.
func (j *jobWriter) compressChunk(n int, end bool) error {
	var src unsafe.Pointer
	if n > 0 {
		src = unsafe.Pointer(&j.job[j.chunked])
	}
	var rc C.size_t
	if end {
		rc = C.ZSTD_compressEnd(j.cctx, j.out, C.size_t(j.outCap), src, C.size_t(n))
	} else {
		rc = C.ZSTD_compressContinue(j.cctx, j.out, C.size_t(j.outCap), src, C.size_t(n))
	}
	if C.ZSTD_isError(rc) != 0 {
		j.err = fmt.Errorf("libzstd: compress: %s", C.GoString(C.ZSTD_getErrorName(rc)))
		return j.err
	}
	j.chunked += n
	out := unsafe.Slice((*byte)(j.out), int(rc))
	j.emitted = append(j.emitted, out...)
	if _, err := j.w.Write(out); err != nil {
		j.err = err
		return err
	}
	return nil
}

// Write buffers p into the first job, compressing each chunk once input
// follows it (the last chunk of the frame is compressed by Close, with
// the end directive), and hands everything past the job to libzstd's own
// compressor.
func (j *jobWriter) Write(p []byte) (int, error) {
	if j.err != nil {
		return 0, j.err
	}
	if j.closed {
		return 0, errors.New("libzstd: write after close")
	}
	total := 0
	for len(p) > 0 {
		if j.direct != nil {
			n, err := j.direct.Write(p)
			if err != nil {
				j.err = err
			}
			return total + n, err
		}
		if len(j.job) == j.jobSize {
			// More input after a full job: the job is not the last one.
			// Compress what is left of it, then replay it into libzstd's
			// compressor and let that take over.
			if err := j.handOver(); err != nil {
				return total, err
			}
			continue
		}
		n := min(j.jobSize-len(j.job), len(p))
		j.job = append(j.job, p[:n]...)
		p = p[n:]
		total += n
		for len(j.job)-j.chunked > jobChunk {
			if err := j.compressChunk(jobChunk, false); err != nil {
				return total, err
			}
		}
	}
	return total, nil
}

// handOver finishes the first job with the buffer-less API and starts
// libzstd's job-based compressor on the same input.
func (j *jobWriter) handOver() error {
	for j.chunked < len(j.job) {
		if err := j.compressChunk(min(jobChunk, len(j.job)-j.chunked), false); err != nil {
			return err
		}
	}
	j.freeContext()
	j.replayed = &replayCheck{w: j.w, want: j.emitted}
	direct, err := newDirectWriter(j.replayed, j.p, j.size)
	if err != nil {
		j.err = err
		return err
	}
	j.direct = direct
	_, err = direct.Write(j.job)
	j.freeJob()
	if err != nil {
		j.err = err
		return err
	}
	return nil
}

func (j *jobWriter) Close() error {
	if j.closed {
		return j.err
	}
	j.closed = true
	if j.direct != nil {
		// libzstd's compressor took over: close it whatever happened, so
		// that it releases its memory, and expect it to have written at
		// least the first job.
		err := j.direct.Close()
		if j.err == nil {
			j.err = err
		}
		if j.err == nil && j.replayed.pos < len(j.replayed.want) {
			j.err = errReplay
		}
		return j.err
	}
	defer j.freeEmulation()
	if j.err != nil {
		return j.err
	}
	// The whole input is one job, the last one: every chunk but the last
	// continues, the last ends the frame.
	for len(j.job)-j.chunked > jobChunk {
		if err := j.compressChunk(jobChunk, false); err != nil {
			return err
		}
	}
	return j.compressChunk(len(j.job)-j.chunked, true)
}

func (j *jobWriter) freeEmulation() {
	j.freeContext()
	j.freeJob()
}

func (j *jobWriter) freeContext() {
	if j.cctx != nil {
		C.ZSTD_freeCCtx(j.cctx)
		j.cctx = nil
	}
	if j.out != nil {
		C.free(j.out)
		j.out = nil
	}
}

func (j *jobWriter) freeJob() {
	if j.jobBuf != nil {
		C.free(j.jobBuf)
		j.jobBuf = nil
		j.job = nil
	}
}

// replayCheck passes a writer's output on once the first len(want) bytes
// of it have been checked against want.
type replayCheck struct {
	w    io.Writer
	want []byte
	pos  int
}

func (r *replayCheck) Write(p []byte) (int, error) {
	n := len(p)
	if r.pos < len(r.want) {
		k := min(len(r.want)-r.pos, len(p))
		if !bytes.Equal(p[:k], r.want[r.pos:r.pos+k]) {
			return 0, errReplay
		}
		r.pos += k
		p = p[k:]
	}
	if len(p) > 0 {
		if _, err := r.w.Write(p); err != nil {
			return 0, err
		}
	}
	return n, nil
}
