//go:build cgo

package pigz

import (
	"bufio"
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "pigz" || !strings.HasPrefix(e.Version(), "2.8+zlib1.") || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s %s", e.Name(), e.Version(), e.Format())
	}
}

func TestCandidateTiers(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 300<<10)
	if len(tiers) != 4 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 10 || tiers[0][0] != (engine.DeflateParams{Level: 9}) || tiers[0][9] != (engine.DeflateParams{Level: 0}) {
		t.Fatalf("tier 1: %+v", tiers[0])
	}
	if len(tiers[1]) != 30 || tiers[1][0] != (engine.DeflateParams{Level: 9, SingleThread: true}) ||
		tiers[1][10] != (engine.DeflateParams{Level: 9, Independent: true}) || tiers[1][20] != (engine.DeflateParams{Level: 9, Rsyncable: true}) {
		t.Fatalf("tier 2: %+v", tiers[1])
	}
	// 300 KiB input: 32, 64 and 256 are below it, 512 is the smallest at
	// or above it and the default is below it, so four block sizes.
	var blocks []int
	for _, p := range tiers[2] {
		if p.Level == 9 {
			blocks = append(blocks, p.BlockSize)
		}
	}
	if fmt.Sprint(blocks) != "[32 64 256 512]" {
		t.Fatalf("tier 3 blocks for 300 KiB: %v", blocks)
	}
	if len(tiers[3]) != 18 || tiers[3][0] != (engine.DeflateParams{Level: 9, Strategy: engine.StrategyHuffmanOnly}) {
		t.Fatalf("tier 4: %+v", tiers[3])
	}
	// Input that fits the default block: only sizes below it.
	blocks = nil
	for _, p := range New().Candidates(&format.GzipHeader{}, 100<<10)[2] {
		if p.Level == 6 {
			blocks = append(blocks, p.BlockSize)
		}
	}
	if fmt.Sprint(blocks) != "[32 64]" {
		t.Fatalf("tier 3 blocks for 100 KiB: %v", blocks)
	}
}

func TestRejectsUnsupportedParams(t *testing.T) {
	for _, p := range []engine.DeflateParams{
		{Level: 10},
		{Level: -1},
		{Level: 6, Strategy: engine.StrategyFiltered},
		{Level: 6, Strategy: engine.StrategyFixed},
		{Level: 6, WindowBits: 15},
		{Level: 6, MemLevel: 8},
		{Level: 6, BlockSize: 16},
		{Level: 6, BlockSize: 1 << 20},
	} {
		if _, err := New().NewWriter(io.Discard, p); err == nil {
			t.Errorf("%+v: expected an error", p)
		}
	}
}

// representative is the subset of parameters the round-trip test builds
// files with; the search still runs over every candidate.
func representative() []engine.DeflateParams {
	var out []engine.DeflateParams
	for _, l := range []int{0, 1, 6, 9} {
		out = append(out,
			engine.DeflateParams{Level: l},
			engine.DeflateParams{Level: l, SingleThread: true},
			engine.DeflateParams{Level: l, Independent: true},
			engine.DeflateParams{Level: l, Rsyncable: true},
			engine.DeflateParams{Level: l, BlockSize: 64},
		)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	enginetest.RoundTripDeflate(t, New(), representative(), fixtures.Small())
}

func compress(t *testing.T, p engine.DeflateParams, data []byte, chunk int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := New().NewWriter(&buf, p)
	if err != nil {
		t.Fatal(err)
	}
	if chunk <= 0 {
		chunk = len(data) + 1
	}
	for len(data) > 0 {
		n := min(chunk, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOutputIndependentOfWriteSize(t *testing.T) {
	data := fixtures.Mixed(300<<10 + 7)
	for _, p := range []engine.DeflateParams{{Level: 6}, {Level: 6, SingleThread: true, Rsyncable: true}, {Level: 0}} {
		want := compress(t, p, data, 0)
		for _, chunk := range []int{1, 7, 4096, 131071, 131072, 131073} {
			if got := compress(t, p, data, chunk); !bytes.Equal(got, want) {
				t.Errorf("%+v: chunk %d differs from a single write", p, chunk)
			}
		}
	}
}

func TestOutputInflates(t *testing.T) {
	for _, f := range fixtures.All() {
		for _, p := range representative() {
			out := compress(t, p, f.Data, 0)
			got, err := io.ReadAll(flate.NewReader(bytes.NewReader(out)))
			if err != nil {
				t.Fatalf("%s %+v: inflate: %v", f.Name, p, err)
			}
			if !bytes.Equal(got, f.Data) {
				t.Fatalf("%s %+v: inflated content differs", f.Name, p)
			}
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n += len(p)
	if f.n > 1000 {
		return 0, errors.New("boom")
	}
	return len(p), nil
}

func TestWriteErrorPropagates(t *testing.T) {
	w, _ := New().NewWriter(&failWriter{}, engine.DeflateParams{Level: 1})
	_, werr := w.Write(fixtures.Random(1<<20, 9))
	cerr := w.Close()
	if werr == nil && cerr == nil {
		t.Fatal("expected the destination's error")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected an error writing after close")
	}
}

func pigzPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("pigz")
	if err != nil {
		t.Skip("pigz not on PATH; run inside the flake dev shell")
	}
	return p
}

// referencePigz compresses a file with the real pigz and returns the raw
// deflate payload.
func referencePigz(t *testing.T, pigz, file string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(pigz, append(append([]string{"-c", "-n"}, args...), file)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("pigz %v: %v: %s", args, err, errb.String())
	}
	b := out.Bytes()
	hdr, err := format.ParseGzipHeader(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatal(err)
	}
	return b[len(hdr.Raw) : len(b)-8]
}

// variant pairs pigz flags with the parameters that should reproduce them.
type variant struct {
	args []string
	p    engine.DeflateParams
}

func variants() []variant {
	var out []variant
	level := func(l int) string { return fmt.Sprintf("-%d", l) }
	for l := 0; l <= 9; l++ {
		out = append(out,
			variant{[]string{"-p4", level(l)}, engine.DeflateParams{Level: l}},
			variant{[]string{"-p1", level(l)}, engine.DeflateParams{Level: l, SingleThread: true}},
		)
	}
	for _, l := range []int{1, 6} {
		for _, path := range []struct {
			arg    string
			single bool
		}{{"-p4", false}, {"-p1", true}} {
			base := engine.DeflateParams{Level: l, SingleThread: path.single}
			with := func(f func(*engine.DeflateParams)) engine.DeflateParams { p := base; f(&p); return p }
			out = append(out,
				variant{[]string{path.arg, level(l), "-i"}, with(func(p *engine.DeflateParams) { p.Independent = true })},
				variant{[]string{path.arg, level(l), "-R"}, with(func(p *engine.DeflateParams) { p.Rsyncable = true })},
				variant{[]string{path.arg, level(l), "-i", "-R"}, with(func(p *engine.DeflateParams) { p.Independent = true; p.Rsyncable = true })},
				variant{[]string{path.arg, level(l), "-b", "32"}, with(func(p *engine.DeflateParams) { p.BlockSize = 32 })},
				variant{[]string{path.arg, level(l), "-b", "64"}, with(func(p *engine.DeflateParams) { p.BlockSize = 64 })},
				variant{[]string{path.arg, level(l), "-b", "256"}, with(func(p *engine.DeflateParams) { p.BlockSize = 256 })},
				variant{[]string{path.arg, level(l), "-b", "32", "-R"}, with(func(p *engine.DeflateParams) { p.BlockSize = 32; p.Rsyncable = true })},
				variant{[]string{path.arg, level(l), "-H"}, with(func(p *engine.DeflateParams) { p.Strategy = engine.StrategyHuffmanOnly })},
				variant{[]string{path.arg, level(l), "-U"}, with(func(p *engine.DeflateParams) { p.Strategy = engine.StrategyRLE })},
			)
		}
	}
	return out
}

// boundaryInputs are sizes around pigz's block boundaries.
func boundaryInputs() []fixtures.Fixture {
	sizes := []int{0, 1, 100, 32767, 32768, 32769, 65536, 131071, 131072, 131073,
		163840, 200000, 262144, 262145, 400000}
	var out []fixtures.Fixture
	for _, n := range sizes {
		out = append(out,
			fixtures.Fixture{Name: fmt.Sprintf("text-%d", n), Data: fixtures.Text(n)},
			fixtures.Fixture{Name: fmt.Sprintf("zeros-%d", n), Data: fixtures.Zeros(n)},
			fixtures.Fixture{Name: fmt.Sprintf("mixed-%d", n), Data: fixtures.Mixed(n)},
		)
	}
	return out
}

func TestMatchesPigz(t *testing.T) {
	pigz := pigzPath(t)
	dir := t.TempDir()
	inputs := append(fixtures.All(), boundaryInputs()...)
	for _, f := range inputs {
		src := filepath.Join(dir, f.Name)
		if err := os.WriteFile(src, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		for _, v := range variants() {
			t.Run(f.Name+"/"+strings.Join(v.args, " "), func(t *testing.T) {
				want := referencePigz(t, pigz, src, v.args...)
				got := compress(t, v.p, f.Data, 0)
				if !bytes.Equal(got, want) {
					t.Fatalf("output differs from pigz %v: got %d bytes, want %d (first difference at %d)", v.args, len(got), len(want), firstDiff(got, want))
				}
			})
		}
	}
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
