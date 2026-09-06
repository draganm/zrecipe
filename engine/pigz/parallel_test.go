//go:build cgo

package pigz

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/fixtures"
)

// compressWith is compress with a caller-supplied engine.
func compressWith(t testing.TB, e *Engine, p engine.DeflateParams, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := e.NewWriter(&buf, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// jobShapes are the parameter sets that change how jobs are cut and primed:
// the dictionary chain, --independent, --rsyncable, other block sizes and
// the strategies.
func jobShapes(level int) []engine.DeflateParams {
	out := []engine.DeflateParams{
		{Level: level},
		{Level: level, Independent: true},
		{Level: level, Rsyncable: true},
		{Level: level, Independent: true, Rsyncable: true},
		{Level: level, BlockSize: 32},
		{Level: level, BlockSize: 32, Rsyncable: true},
		{Level: level, BlockSize: 512},
	}
	if level > 0 {
		out = append(out,
			engine.DeflateParams{Level: level, Strategy: engine.StrategyHuffmanOnly},
			engine.DeflateParams{Level: level, Strategy: engine.StrategyRLE})
	}
	return out
}

func TestWorkersDoNotChangeOutput(t *testing.T) {
	seq := &Engine{Workers: 1}
	par := &Engine{Workers: 7}
	inputs := append(boundaryInputs(),
		fixtures.Fixture{Name: "mixed-4m+3", Data: fixtures.Mixed(4<<20 + 3)},
		fixtures.Fixture{Name: "text-2m+1", Data: fixtures.Text(2<<20 + 1)},
		fixtures.Fixture{Name: "random-1m", Data: fixtures.Random(1<<20, 5)},
	)
	for _, f := range inputs {
		levels := []int{0, 1, 6, 9}
		if len(f.Data) > 1<<20 {
			levels = []int{0, 1, 6}
		}
		for _, l := range levels {
			for _, p := range jobShapes(l) {
				want := compressWith(t, seq, p, f.Data)
				got := compressWith(t, par, p, f.Data)
				if !bytes.Equal(got, want) {
					t.Errorf("%s %+v: %d workers differ from one: got %d bytes, want %d (first difference at %d)",
						f.Name, p, par.Workers, len(got), len(want), firstDiff(got, want))
				}
			}
		}
	}
}

func TestDefaultWorkersIsGOMAXPROCS(t *testing.T) {
	if got := New().workers(); got != runtime.GOMAXPROCS(0) {
		t.Fatalf("workers() = %d, want GOMAXPROCS %d", got, runtime.GOMAXPROCS(0))
	}
	if got := (&Engine{Workers: 3}).workers(); got != 3 {
		t.Fatalf("workers() = %d, want 3", got)
	}
}

func TestWorkersCompressBlocksConcurrently(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if raceEnabled {
		t.Skip("timing is not comparable under the race detector")
	}
	if runtime.NumCPU() < 4 {
		t.Skip("needs at least four CPUs")
	}
	data := fixtures.Mixed(16 << 20)
	p := engine.DeflateParams{Level: 9}
	timed := func(e *Engine) time.Duration {
		start := time.Now()
		compressWith(t, e, p, data)
		return time.Since(start)
	}
	one := timed(&Engine{Workers: 1})
	four := timed(&Engine{Workers: 4})
	if four > one*8/10 {
		t.Fatalf("four workers took %v, one took %v; blocks are not compressed concurrently", four, one)
	}
}

func BenchmarkWorkers(b *testing.B) {
	data := fixtures.Mixed(8 << 20)
	for _, n := range []int{1, 2, 4, 8} {
		e := &Engine{Workers: n}
		b.Run(string(rune('0'+n)), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for range b.N {
				compressWith(b, e, engine.DeflateParams{Level: 6}, data)
			}
		})
	}
}

// errMismatch is failFirstWrite's error.
var errMismatch = errors.New("mismatch")

// failFirstWrite rejects every write, like a search candidate whose first
// block already differs from the reference.
type failFirstWrite struct{}

func (failFirstWrite) Write([]byte) (int, error) { return 0, errMismatch }

// cpuTime is the process's user plus system CPU time, cgo threads included.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// TestMismatchCostsOneJob checks that a destination rejecting the first
// write, which is what the candidate search does to a candidate whose
// first block differs, costs about one job of CPU time rather than one per
// worker: nothing past the first job is compressed until that job's output
// has been accepted. The search tries hundreds of candidates per input, so
// a mismatch has to stay as cheap as it was with one stream.
func TestMismatchCostsOneJob(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if raceEnabled {
		t.Skip("CPU time is not comparable under the race detector")
	}
	data := fixtures.Random(48<<20, 8)
	p := engine.DeflateParams{Level: 6, BlockSize: 4096} // 12 jobs of 4 MiB
	e := &Engine{Workers: 8}

	before := cpuTime(t)
	compressWith(t, e, p, data)
	full := cpuTime(t) - before

	before = cpuTime(t)
	w, err := e.NewWriter(failFirstWrite{}, p)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := w.Write(data)
	cerr := w.Close()
	failed := cpuTime(t) - before
	if werr == nil && cerr == nil {
		t.Fatal("expected the destination's error")
	}
	// The full run compresses twelve jobs; the failing one may compress
	// the first, and with eight workers busy from the start it would
	// compress eight.
	if failed > full/4 {
		t.Fatalf("a mismatch on the first block cost %v of CPU, the whole input costs %v", failed, full)
	}
}

// TestMismatchCostsLessThanFirstJob checks that a destination rejecting the
// first write costs a small part of the first job's CPU time, not all of
// it: the first job's output goes to the destination as deflate produces
// it, so a candidate whose first block differs is rejected once that block
// is done. With the largest block size the first job is 4 MiB of input,
// all of which a wrong candidate used to compress before it was rejected.
func TestMismatchCostsLessThanFirstJob(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	if raceEnabled {
		t.Skip("CPU time is not comparable under the race detector")
	}
	data := fixtures.Random(4<<20, 8)
	p := engine.DeflateParams{Level: 9, BlockSize: 4096} // one job of 4 MiB
	e := New()

	before := cpuTime(t)
	w, err := e.NewWriter(io.Discard, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	full := cpuTime(t) - before

	before = cpuTime(t)
	w, err = e.NewWriter(failFirstWrite{}, p)
	if err != nil {
		t.Fatal(err)
	}
	_, werr := w.Write(data)
	cerr := w.Close()
	failed := cpuTime(t) - before
	if werr == nil && cerr == nil {
		t.Fatal("expected the destination's error")
	}
	if failed > full/4 {
		t.Fatalf("a mismatch on the first block cost %v of CPU, the whole first job costs %v", failed, full)
	}
}

// TestDirectJobWriteError checks that the destination's error while the
// first job streams to it is what Close returns, whether that job is the
// whole input or more jobs are cut behind it, and that the writer does not
// wait for output that will never be accepted.
func TestDirectJobWriteError(t *testing.T) {
	for _, size := range []int{100 << 10, 1 << 20} {
		w, err := (&Engine{Workers: 4}).NewWriter(failFirstWrite{}, engine.DeflateParams{Level: 1})
		if err != nil {
			t.Fatal(err)
		}
		type result struct{ write, close error }
		res := make(chan result, 1)
		go func() {
			_, werr := w.Write(fixtures.Random(size, 13))
			res <- result{werr, w.Close()}
		}()
		var r result
		select {
		case r = <-res:
		case <-time.After(time.Minute):
			t.Fatalf("%d bytes: Write and Close did not return after the destination rejected the first job", size)
		}
		if !errors.Is(r.close, errMismatch) {
			t.Fatalf("%d bytes: Close returned %v, want the destination's error", size, r.close)
		}
		if r.write != nil && !errors.Is(r.write, errMismatch) {
			t.Fatalf("%d bytes: Write returned %v, want nil or the destination's error", size, r.write)
		}
	}
}

// compressJobWith compresses one job on a fresh compressor, its output
// streamed in chunks of stream bytes (0 buffers it whole, as a later job
// is), and returns the bytes.
func compressJobWith(t *testing.T, stream, level int, strategy string, setdict bool, data, dict []byte, lens []int, more bool) []byte {
	t.Helper()
	c, err := newCompressor(nil, level, strategies[strategy], defaultBlock<<10, setdict, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c.free()
	var buf bytes.Buffer
	c.w, c.stream = &buf, stream
	var d *space
	if setdict {
		d = &space{buf: dict, n: len(dict)}
	}
	if err := c.compressJob(&space{buf: data, n: len(data)}, lens, lens != nil, d, more); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestStreamedJobMatchesBuffered checks what streaming the first job relies
// on: the bytes a job produces do not depend on how much output space each
// deflate call gets, so a job streamed in chunks of any size, down to one
// byte, where every flush ends with the space exactly full and deflate is
// called once more, is the job compressed into a buffer. Level 0, where
// zlib sizes stored blocks by the space, is included because engine has to
// leave it alone.
func TestStreamedJobMatchesBuffered(t *testing.T) {
	text := fixtures.Text(80000)
	const n = 20000
	inputs := []fixtures.Fixture{
		{Name: "text", Data: text[:n]},
		{Name: "mixed", Data: fixtures.Mixed(n)},
		{Name: "random", Data: fixtures.Random(n, 3)},
		{Name: "zeros", Data: fixtures.Zeros(n)},
		{Name: "short", Data: text[:100]},
	}
	dict := text[len(text)-40000:] // the previous block; the last 32 KiB prime the job
	shapes := []engine.DeflateParams{
		{Level: 0}, {Level: 1}, {Level: 6}, {Level: 9},
		{Level: 6, Strategy: engine.StrategyHuffmanOnly}, {Level: 6, Strategy: engine.StrategyRLE},
	}
	for _, f := range inputs {
		for _, p := range shapes {
			for _, setdict := range []bool{true, false} {
				for _, more := range []bool{true, false} {
					// With and without rsync cut points, which make a job flush more than once.
					for _, lens := range [][]int{nil, {len(f.Data) / 3, len(f.Data) / 2}} {
						want := compressJobWith(t, 0, p.Level, p.Strategy, setdict, f.Data, dict, lens, more)
						for _, chunk := range []int{1, 7, 1000, streamChunk} {
							got := compressJobWith(t, chunk, p.Level, p.Strategy, setdict, f.Data, dict, lens, more)
							if !bytes.Equal(got, want) {
								t.Errorf("%s %+v setdict=%v more=%v lens=%v: streamed in %d-byte chunks differs from buffered: got %d bytes, want %d (first difference at %d)",
									f.Name, p, setdict, more, lens, chunk, len(got), len(want), firstDiff(got, want))
							}
						}
					}
				}
			}
		}
	}
}
