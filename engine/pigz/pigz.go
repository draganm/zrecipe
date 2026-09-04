//go:build cgo

// Package pigz reproduces the raw deflate streams that pigz, the parallel
// gzip, writes. pigz compresses with zlib but cuts the input into blocks
// (128 KiB by default), restarts the compressor on every block primed with
// the previous 32 KiB, and byte-aligns each block with empty deflate blocks,
// so a single zlib stream never matches its output past the first block.
// This engine drives the system zlib exactly as pigz 2.8 does, including
// pigz's single-thread code path (-p 1), which keeps one stream but flushes
// at the same boundaries, and its --rsyncable and --independent modes.
package pigz

/*
#cgo pkg-config: zlib
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

#if ZLIB_VERNUM < 0x1260
#error "the pigz engine needs zlib 1.2.6 or later"
#endif

// deflateInit2 is a macro; wrap it so cgo can call it.
static int cp_deflate_init(z_streamp s, int level, int windowBits, int memLevel, int strategy) {
	return deflateInit2(s, level, Z_DEFLATED, windowBits, memLevel, strategy);
}

static int cp_pending_bits(z_streamp s) {
	int bits = 0;
	deflatePending(s, Z_NULL, &bits);
	return bits;
}
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"unsafe"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

const (
	// pigzVersion is the pigz release whose algorithm this package ports.
	pigzVersion = "2.8"

	dictSize     = 32768   // DICT: dictionary size, also the smallest block size
	defaultBlock = 128     // -b default, in KiB
	maxBlockKiB  = 1 << 19 // pigz rejects blocks above 2^29 bytes
	maxP2        = 1 << 31 // MAXP2: the most input pigz hands deflate at once
	rsyncBits    = 12      // RSYNCBITS
	rsyncMask    = (1 << rsyncBits) - 1
	rsyncHit     = rsyncMask >> 1
)

var strategies = map[string]C.int{
	"":                         C.Z_DEFAULT_STRATEGY,
	engine.StrategyDefault:     C.Z_DEFAULT_STRATEGY,
	engine.StrategyHuffmanOnly: C.Z_HUFFMAN_ONLY, // -H
	engine.StrategyRLE:         C.Z_RLE,          // -U
}

// otherBlocks are the block sizes, in KiB, tried after the default.
var otherBlocks = []int{32, 64, 256, 512, 1024, 2048, 4096}

// Engine produces raw deflate streams the way pigz does.
type Engine struct {
	// Workers is how many blocks the parallel path compresses at once, each
	// on its own zlib stream, as pigz -p does; 0 means GOMAXPROCS. The
	// output does not depend on it.
	Workers int
}

// workers is the parallel path's worker count.
func (e *Engine) workers() int {
	if e.Workers > 0 {
		return e.Workers
	}
	return runtime.GOMAXPROCS(0)
}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "pigz" }
func (*Engine) Version() string       { return pigzVersion + "+zlib" + C.GoString(C.zlibVersion()) }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns four tiers: the default block on the parallel path;
// the single-thread path, --independent and --rsyncable; other block sizes
// that can change the output for this input size; and pigz's two strategy
// options.
func (*Engine) Candidates(h *format.GzipHeader, size int64) [][]engine.DeflateParams {
	levels := engine.LevelOrder(h.XFL)
	var t1, t2, t3, t4 []engine.DeflateParams
	for _, l := range levels {
		t1 = append(t1, engine.DeflateParams{Level: l})
	}
	for _, l := range levels {
		t2 = append(t2, engine.DeflateParams{Level: l, SingleThread: true})
	}
	for _, l := range levels {
		t2 = append(t2, engine.DeflateParams{Level: l, Independent: true})
	}
	for _, l := range levels {
		t2 = append(t2, engine.DeflateParams{Level: l, Rsyncable: true})
	}
	// A block no smaller than the input gives one job, and every such block
	// gives the same bytes; the default already covers that when it is not
	// below the input, otherwise one representative is enough.
	needOne := size > defaultBlock<<10
	for _, b := range otherBlocks {
		bytes := int64(b) << 10
		if bytes >= size {
			if !needOne {
				continue
			}
			needOne = false
		}
		for _, l := range levels {
			t3 = append(t3, engine.DeflateParams{Level: l, BlockSize: b})
		}
	}
	for _, s := range []string{engine.StrategyHuffmanOnly, engine.StrategyRLE} {
		for _, l := range levels {
			if l == 0 {
				continue
			}
			t4 = append(t4, engine.DeflateParams{Level: l, Strategy: s})
		}
	}
	return [][]engine.DeflateParams{t1, t2, t3, t4}
}

// NewWriter returns a writer that compresses into w. Close flushes.
func (e *Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Level < 0 || p.Level > 9 {
		return nil, fmt.Errorf("pigz: level %d out of range 0..9", p.Level)
	}
	st, ok := strategies[p.Strategy]
	if !ok {
		return nil, fmt.Errorf("pigz: strategy %q is not one pigz offers (huffman_only, rle)", p.Strategy)
	}
	if p.WindowBits != 0 || p.MemLevel != 0 {
		return nil, errors.New("pigz: window_bits and mem_level are not supported")
	}
	block := p.BlockSize
	if block == 0 {
		block = defaultBlock
	}
	if block < dictSize>>10 || block > maxBlockKiB {
		return nil, fmt.Errorf("pigz: block_size %d KiB out of range %d..%d", block, dictSize>>10, maxBlockKiB)
	}
	c, err := newCompressor(w, C.int(p.Level), st, block<<10, !p.Independent, p.Rsyncable)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	wr := &writer{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(wr.done)
		defer c.free()
		var err error
		if p.SingleThread {
			err = c.single(pr)
		} else {
			err = c.parallel(pr, e.workers())
		}
		if err != nil {
			wr.err = err
			pr.CloseWithError(err)
			return
		}
		pr.Close()
	}()
	return wr, nil
}

// writer feeds the compressor goroutine through a pipe.
type writer struct {
	pw     *io.PipeWriter
	done   chan struct{}
	err    error // set by the goroutine before done is closed
	closed bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, errors.New("pigz: write after close")
	}
	return w.pw.Write(p)
}

func (w *writer) Close() error {
	if w.closed {
		return w.err
	}
	w.closed = true
	w.pw.Close()
	<-w.done
	return w.err
}

// space is pigz's struct space: a buffer and how much of it is in use.
type space struct {
	buf []byte
	n   int
}

// outSpace tracks the size and fill of a job's output buffer, which decides
// avail_out on each deflate call and with it zlib's stored-block lengths.
type outSpace struct {
	size, n int
}

// outPool is OUTPOOL: the initial size of a job's output buffer.
func outPool(block int) int { return block + block>>4 + dictSize }

// grow is pigz's grow(): the next size up for a buffer that ran out.
func grow(size int) int {
	size += size >> 2
	top := size
	shift := 0
	for top > 7 {
		top >>= 1
		shift++
	}
	if top == 7 {
		size = 1 << (shift + 3)
	}
	if size < 16 {
		size = 16
	}
	return size
}

// readn is pigz's readn: fill buf completely unless the input ends first.
func readn(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return n, nil
	}
	return n, err
}

// compressor holds the zlib stream and the C buffers behind it.
type compressor struct {
	w        io.Writer
	level    C.int
	strategy C.int
	block    int // bytes
	setdict  bool
	rsync    bool

	strm    *C.z_stream
	cin     unsafe.Pointer // block bytes: the piece being compressed
	cdict   unsafe.Pointer // dictSize bytes
	cout    unsafe.Pointer
	coutCap int
}

func newCompressor(w io.Writer, level, strategy C.int, block int, setdict, rsync bool) (*compressor, error) {
	strm := (*C.z_stream)(C.calloc(1, C.size_t(unsafe.Sizeof(C.z_stream{}))))
	if strm == nil {
		return nil, errors.New("pigz: out of memory")
	}
	// pigz initializes at level 6 and sets the real level with
	// deflateParams after each deflateReset.
	if rc := C.cp_deflate_init(strm, 6, -15, 8, strategy); rc != C.Z_OK {
		C.free(unsafe.Pointer(strm))
		return nil, fmt.Errorf("pigz: deflateInit2 returned %d", rc)
	}
	c := &compressor{w: w, level: level, strategy: strategy, block: block, setdict: setdict, rsync: rsync, strm: strm}
	c.cin = C.malloc(C.size_t(block))
	c.cdict = C.malloc(dictSize)
	c.coutCap = outPool(block)
	c.cout = C.malloc(C.size_t(c.coutCap))
	return c, nil
}

func (c *compressor) free() {
	C.deflateEnd(c.strm)
	C.free(c.cin)
	C.free(c.cdict)
	C.free(c.cout)
	C.free(unsafe.Pointer(c.strm))
}

// setInput hands deflate one piece of input in a single call, as pigz does
// (its MAXP2 loop never runs for blocks within pigz's limits).
func (c *compressor) setInput(p []byte) {
	if len(p) > 0 {
		C.memcpy(c.cin, unsafe.Pointer(&p[0]), C.size_t(len(p)))
	}
	c.strm.next_in = (*C.Bytef)(c.cin)
	c.strm.avail_in = C.uInt(len(p))
}

// deflateOut runs one deflate call with room bytes of output space and
// writes whatever it produced. It returns the space left over.
func (c *compressor) deflateOut(flush C.int, room int) (int, error) {
	if room > c.coutCap {
		c.cout = C.realloc(c.cout, C.size_t(room))
		c.coutCap = room
	}
	c.strm.next_out = (*C.Bytef)(c.cout)
	c.strm.avail_out = C.uInt(room)
	C.deflate(c.strm, flush) // pigz ignores the return code too
	avail := int(c.strm.avail_out)
	if produced := room - avail; produced > 0 {
		if _, err := c.w.Write(unsafe.Slice((*byte)(c.cout), produced)); err != nil {
			return 0, err
		}
	}
	return avail, nil
}

// engine is deflate_engine: run deflate into a job's output space, growing
// the space when it fills, until deflate has nothing more to write.
func (c *compressor) engine(out *outSpace, flush C.int) error {
	for {
		room := out.size - out.n
		if room == 0 {
			out.size = grow(out.size)
			room = out.size - out.n
		}
		avail, err := c.deflateOut(flush, room)
		if err != nil {
			return err
		}
		out.n += room - avail
		if avail != 0 {
			return nil
		}
	}
}

// endPiece runs the flush sequence both code paths share once a piece's
// input is set: finish the stream if nothing follows, otherwise end the
// deflate block and get to a byte boundary with a sync marker (odd pending
// bit count, or --independent) or with empty static blocks primed ten bits
// at a time. afterPrime is the flush pigz uses to push the primed bits out:
// Z_BLOCK on the parallel path, Z_NO_FLUSH on the single-thread path.
func (c *compressor) endPiece(run func(flush C.int) error, more bool, afterPrime C.int) error {
	if !more {
		return run(C.Z_FINISH)
	}
	if err := run(C.Z_BLOCK); err != nil {
		return err
	}
	bits := int(C.cp_pending_bits(c.strm))
	if bits&1 != 0 || !c.setdict {
		if err := run(C.Z_SYNC_FLUSH); err != nil {
			return err
		}
	} else if bits&7 != 0 {
		for {
			C.deflatePrime(c.strm, 10, 2) // an empty static block
			if bits = int(C.cp_pending_bits(c.strm)); bits&7 == 0 {
				break
			}
		}
		if err := run(afterPrime); err != nil {
			return err
		}
	}
	if !c.setdict { // two markers when independent
		return run(C.Z_FULL_FLUSH)
	}
	return nil
}

// job is one of parallel_compress's jobs: a block of input, the dictionary
// it is primed with, the rsync cut points within it and whether more input
// follows. The worker that compresses it leaves the output and any error
// behind and closes done.
type job struct {
	curr    *space
	lens    []int
	hasLens bool
	dict    *space
	more    bool
	out     bytes.Buffer
	err     error
	done    chan struct{}
}

// errStopped ends job cutting once the output side has failed; the
// output's error is what parallel reports.
var errStopped = errors.New("pigz: stopped")

// parallel is parallel_compress and compress_thread: jobs are cut from the
// input in order on this goroutine (cutJobs) and compressed by workers
// goroutines, each on its own zlib stream, and their output is written in
// job order. A job is compressed from nothing but its own input and
// dictionary, so the output does not depend on how jobs are scheduled.
//
// The first job runs alone, and the other workers are created only once
// its output has been accepted: the candidate search feeds hundreds of
// parameter sets through here and rejects most of them at their first
// block, and a rejected candidate must cost one job, not one per worker.
func (c *compressor) parallel(r io.Reader, workers int) error {
	if workers < 1 {
		workers = 1
	}
	out := c.w // c itself is worker 0 and writes into job buffers from here
	// jobs is unbuffered so that only the jobs being compressed are in
	// flight: once the output fails, what is left to finish is one job per
	// worker rather than a queue of them.
	jobs := make(chan *job)
	order := make(chan *job, 2*workers)
	stop := make(chan struct{})
	var stopOnce sync.Once
	fail := func() { stopOnce.Do(func() { close(stop) }) }
	accepted := make(chan struct{}) // closed once the first job's output is written

	var wg sync.WaitGroup
	start := func(s *compressor) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				select {
				case <-stop:
					// The output has failed; nothing more is written.
				default:
					s.w = &j.out
					j.err = s.compressJob(j.curr, j.lens, j.hasLens, j.dict, j.more)
				}
				close(j.done)
			}
		}()
	}
	start(c)
	written := make(chan error, 1)
	go func() {
		var err error
		first := true
		for j := range order {
			<-j.done
			if err != nil {
				continue
			}
			if err = j.err; err == nil {
				_, err = out.Write(j.out.Bytes())
			}
			if err != nil {
				fail()
				continue
			}
			if first {
				first = false
				close(accepted)
			}
		}
		written <- err
	}()

	var extra []*compressor
	defer func() {
		for _, s := range extra {
			s.free()
		}
	}()
	cut := 0
	// A job is handed to the workers before it is queued for output, so
	// every job the writer waits for is one a worker will finish.
	err := c.cutJobs(r, func(j *job) error {
		if cut == 1 {
			select {
			case <-accepted:
			case <-stop:
				return errStopped
			}
			for len(extra) < workers-1 {
				s, err := newCompressor(nil, c.level, c.strategy, c.block, c.setdict, c.rsync)
				if err != nil {
					return err
				}
				extra = append(extra, s)
				start(s)
			}
		}
		cut++
		j.done = make(chan struct{})
		select {
		case jobs <- j:
		case <-stop:
			return errStopped
		}
		select {
		case order <- j:
		case <-stop:
			return errStopped
		}
		return nil
	})
	close(jobs)
	wg.Wait()
	close(order)
	if werr := <-written; werr != nil {
		return werr
	}
	return err
}

// cutJobs is the job cutting of parallel_compress: it reads the input and
// hands every job to emit in order. The input buffer juggling is kept as
// pigz has it because it decides where jobs end, in particular at end of
// input, where the bytes after the last rsync hit stay in the same job.
// A job's buffers are not touched after it is emitted.
func (c *compressor) cutJobs(r io.Reader, emit func(*job) error) error {
	newSpace := func() *space { return &space{buf: make([]byte, c.block)} }
	hash := uint32(rsyncHit)
	var (
		scan int // index of the next byte to hash, in curr or next as pigz's pointer would be
		left int // last hit in curr to end of curr
		hold *space
		dict *space // dictionary for the next job
	)
	next := newSpace()
	n, err := readn(r, next.buf)
	if err != nil {
		return err
	}
	next.n = n
	for {
		curr := next
		next = hold
		hold = nil
		if next == nil {
			next = newSpace()
			n, err := readn(r, next.buf)
			if err != nil {
				return err
			}
			next.n = n
		}

		// If rsyncable, generate block lengths and prepare curr for the job
		// to likely have less than a block (up to the last hash hit).
		var lens []int
		hasLens := false
		if c.rsync && curr.n > 0 {
			hasLens = true
			if left == 0 {
				// scan is in curr
				last := 0
				for scan < curr.n {
					hash = ((hash << 1) ^ uint32(curr.buf[scan])) & rsyncMask
					scan++
					if hash == rsyncHit {
						lens = append(lens, scan-last)
						last = scan
					}
				}
				left = scan - last
				scan = 0 // continue scan in next
			}
			// Scan in next for enough bytes to fill curr, or what is
			// available in next, whichever is less; the bytes in curr since
			// the last hit count towards the first block.
			last := 0
			end := len(curr.buf) - curr.n
			if end > next.n {
				end = next.n
			}
			for scan < end {
				hash = ((hash << 1) ^ uint32(next.buf[scan])) & rsyncMask
				scan++
				if hash == rsyncHit {
					lens = append(lens, scan-last+left)
					left = 0
					last = scan
				}
			}
			// Create input in curr for the job up to the last hit, or the
			// entire buffer if no hits at all; save the remainder in next
			// and possibly hold.
			cut := last
			if len(lens) == 0 {
				cut = scan
			}
			if cut > 0 {
				// hits in next, or no hits in either: copy to curr
				copy(curr.buf[curr.n:], next.buf[:cut])
				curr.n += cut
				copy(next.buf, next.buf[cut:next.n])
				next.n -= cut
				scan -= cut
				left = 0
			} else if len(lens) != 0 && left != 0 && next.n != 0 {
				// hits in curr but none in next, and the last hit was not
				// at the end: use curr up to the last hit, save the rest,
				// move next to hold
				hold = next
				next = newSpace()
				copy(next.buf, curr.buf[curr.n-left:curr.n])
				next.n = left
				curr.n -= left
			} else {
				// the last hit was right at the end of curr, or this is the
				// end of the input
				left = 0
			}
		}

		more := next.n != 0

		// Provide the dictionary for this job, prepare the one for the next.
		jobDict := dict
		if more && c.setdict {
			if curr.n >= dictSize || jobDict == nil {
				dict = curr
			} else {
				d := &space{buf: make([]byte, dictSize)}
				n := dictSize - curr.n
				prev := jobDict.buf[:jobDict.n]
				// pigz takes the n bytes before the end of the previous
				// dictionary. If it holds fewer (a short first job followed
				// by a short second one, only possible with --rsyncable on
				// a 32 KiB block) pigz reads before its buffer; take what
				// exists instead.
				if n > len(prev) {
					n = len(prev)
				}
				copy(d.buf, prev[len(prev)-n:])
				copy(d.buf[n:], curr.buf[:curr.n])
				d.n = n + curr.n
				dict = d
			}
		}

		if err := emit(&job{curr: curr, lens: lens, hasLens: hasLens, dict: jobDict, more: more}); err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// compressJob is the per-job part of compress_thread.
func (c *compressor) compressJob(curr *space, lens []int, hasLens bool, dict *space, more bool) error {
	C.deflateReset(c.strm)
	C.deflateParams(c.strm, c.level, c.strategy)
	if dict != nil {
		l := dict.n
		if l > dictSize {
			l = dictSize
		}
		if l > 0 {
			C.memcpy(c.cdict, unsafe.Pointer(&dict.buf[dict.n-l]), C.size_t(l))
		}
		C.deflateSetDictionary(c.strm, (*C.Bytef)(c.cdict), C.uInt(l))
	}
	out := &outSpace{size: outPool(c.block)}
	run := func(flush C.int) error { return c.engine(out, flush) }

	// Compress each block, either flushing or finishing.
	left := curr.n
	pos := 0
	i := 0
	for {
		n := left // end of list, or no list at all
		if hasLens && i < len(lens) {
			n = lens[i]
			i++
		}
		left -= n
		c.setInput(curr.buf[pos : pos+n])
		pos += n
		if err := c.endPiece(run, left != 0 || more, C.Z_BLOCK); err != nil {
			return err
		}
		if left == 0 {
			return nil
		}
	}
}

// single is single_compress: one stream for the whole input, flushed at
// the same block boundaries as the parallel path, with a reset every block
// of input in --independent mode.
func (c *compressor) single(r io.Reader) error {
	in := make([]byte, c.block+dictSize)
	next := make([]byte, c.block+dictSize)
	outSize := c.block
	if outSize > maxP2 {
		outSize = maxP2
	}
	run := func(flush C.int) error {
		for {
			avail, err := c.deflateOut(flush, outSize)
			if err != nil {
				return err
			}
			if avail != 0 {
				return nil
			}
		}
	}

	C.deflateReset(c.strm)
	C.deflateParams(c.strm, c.level, c.strategy)

	got := 0 // amount of data in in
	more, err := readn(r, next[:c.block])
	if err != nil {
		return err
	}
	start := 0 // start of data in next
	have := 0  // bytes in current block for -i
	hash := uint32(rsyncHit)
	nextIn := 0 // strm->next_in as an index into in
	for {
		// get data to compress, see if there is any more input
		if got == 0 {
			in, next = next, in
			nextIn = start
			got = more
			start = 0
			more, err = readn(r, next[start:start+c.block])
			if err != nil {
				return err
			}
		}

		// if rsyncable, compute the hash until a hit or the end of the block
		left := 0
		if c.rsync && got > 0 {
			sc := nextIn
			left = got
			for {
				if left == 0 {
					// went to the end: if no more or no hit in a block's
					// worth, flush or finish with got bytes
					if more == 0 || got == c.block {
						break
					}
					// fill in with what's left there and as much as
					// possible from next, and continue the search
					copy(in, in[nextIn:nextIn+got])
					nextIn = 0
					sc = got
					left = more
					if left > c.block-got {
						left = c.block - got
					}
					copy(in[sc:], next[start:start+left])
					got += left
					more -= left
					start += left
					// if that emptied the next buffer, try to refill it
					if more == 0 {
						more, err = readn(r, next[:c.block])
						if err != nil {
							return err
						}
						start = 0
					}
				}
				left--
				hash = ((hash << 1) ^ uint32(in[sc])) & rsyncMask
				sc++
				if hash == rsyncHit {
					break
				}
			}
			got -= left
		}

		// clear history for --independent
		fresh := false
		if !c.setdict {
			have += got
			if have > c.block {
				fresh = true
				have = got
			}
		}
		if fresh {
			C.deflateReset(c.strm)
		}

		// compress the piece, emit a block, finish if end of input
		c.setInput(in[nextIn : nextIn+got])
		nextIn += got
		got = left
		if err := c.endPiece(run, more > 0 || got > 0, C.Z_NO_FLUSH); err != nil {
			return err
		}
		if more == 0 && got == 0 {
			return nil
		}
	}
}
