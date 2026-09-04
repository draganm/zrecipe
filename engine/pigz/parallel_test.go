//go:build cgo

package pigz

import (
	"bytes"
	"errors"
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

// failFirstWrite rejects every write, like a search candidate whose first
// block already differs from the reference.
type failFirstWrite struct{}

func (failFirstWrite) Write([]byte) (int, error) { return 0, errors.New("mismatch") }

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
