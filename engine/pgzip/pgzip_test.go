package pgzip

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
	"strconv"
	"strings"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "pgzip" || e.Version() != "1.2.6+klauspost-compress1.11.3" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s %s", e.Name(), e.Version(), e.Format())
	}
}

func blocksAtLevel(tier []engine.DeflateParams, level int) []int {
	var blocks []int
	for _, p := range tier {
		if p.Level == level {
			blocks = append(blocks, p.BlockSize)
		}
	}
	return blocks
}

func TestCandidateTiers(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 300<<10)
	if len(tiers) != 2 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 11 || tiers[0][0] != (engine.DeflateParams{Level: 9}) || tiers[0][9] != (engine.DeflateParams{Level: 0}) || tiers[0][10] != (engine.DeflateParams{Level: -2}) {
		t.Fatalf("tier 1: %+v", tiers[0])
	}
	// 300 KiB input: 128 and 256 are below it; the default block, larger
	// than the input, already covers every larger size.
	if got := fmt.Sprint(blocksAtLevel(tiers[1], 9)); got != "[128 256]" {
		t.Fatalf("tier 2 blocks for 300 KiB: %v", got)
	}
	// 3 MiB: four sizes not above it, and 4096 stands in for every size
	// above it since the default is below it.
	tiers = New().Candidates(&format.GzipHeader{XFL: 4}, 3<<20)
	if tiers[0][0].Level != 1 {
		t.Fatalf("XFL 4 should try level 1 first: %+v", tiers[0])
	}
	if got := fmt.Sprint(blocksAtLevel(tiers[1], 1)); got != "[128 256 512 2048 4096]" {
		t.Fatalf("tier 2 blocks for 3 MiB: %v", got)
	}
	// Input exactly one default block long fills it and gets an empty
	// block after it, which no larger block does, so one larger size is
	// still needed; and a smaller block exactly the input's size is its
	// own case for the same reason.
	if got := fmt.Sprint(blocksAtLevel(New().Candidates(&format.GzipHeader{}, 1<<20)[1], 5)); got != "[128 256 512 2048]" {
		t.Fatalf("tier 2 blocks for 1 MiB: %v", got)
	}
	if got := fmt.Sprint(blocksAtLevel(New().Candidates(&format.GzipHeader{}, 256<<10)[1], 5)); got != "[128 256]" {
		t.Fatalf("tier 2 blocks for 256 KiB: %v", got)
	}
	// Input that fits every block: nothing to try beyond the default.
	tiers = New().Candidates(&format.GzipHeader{}, 100<<10)
	if tiers[0][0].Level != 5 || tiers[0][1].Level != 6 {
		t.Fatalf("no XFL hint should try klauspost's default level 5 first: %+v", tiers[0])
	}
	if len(tiers[1]) != 0 {
		t.Fatalf("tier 2 for 100 KiB: %+v", tiers[1])
	}
}

func TestRejectsUnsupportedParams(t *testing.T) {
	for _, p := range []engine.DeflateParams{
		{Level: 10},
		{Level: -1},
		{Level: -3},
		{Level: 5, Strategy: engine.StrategyHuffmanOnly},
		{Level: 5, WindowBits: 15},
		{Level: 5, MemLevel: 8},
		{Level: 5, Rsyncable: true},
		{Level: 5, Independent: true},
		{Level: 5, SingleThread: true},
		{Level: 5, BlockSize: 16},
	} {
		if _, err := New().NewWriter(io.Discard, p); err == nil {
			t.Errorf("%+v: expected an error", p)
		}
	}
	if _, err := New().NewWriter(io.Discard, engine.DeflateParams{Level: 5, BlockSize: 17}); err != nil {
		t.Errorf("17 KiB is the smallest block pgzip accepts: %v", err)
	}
}

// representative is the subset of parameters the round-trip test builds
// files with; the search still runs over every candidate.
func representative() []engine.DeflateParams {
	var out []engine.DeflateParams
	for _, l := range []int{-2, 0, 1, 5, 9} {
		out = append(out, engine.DeflateParams{Level: l}, engine.DeflateParams{Level: l, BlockSize: 128})
	}
	return out
}

// roundTripInputs adds inputs that span several default blocks, one of
// them an exact multiple, to the small fixture set.
func roundTripInputs() []fixtures.Fixture {
	return append(fixtures.Small(),
		fixtures.Fixture{Name: "text-1m", Data: fixtures.Text(1 << 20)},
		fixtures.Fixture{Name: "mixed-2m+1", Data: fixtures.Mixed(2<<20 + 1)},
	)
}

func TestRoundTrip(t *testing.T) {
	enginetest.RoundTripDeflate(t, New(), representative(), roundTripInputs())
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
	data := fixtures.Mixed(2<<20 + 7)
	for _, p := range []engine.DeflateParams{{Level: 5}, {Level: 9, BlockSize: 128}, {Level: 0}} {
		want := compress(t, p, data, 0)
		for _, chunk := range []int{1, 7, 4096, 1<<20 - 1, 1 << 20, 1<<20 + 1} {
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
	_, werr := w.Write(fixtures.Random(2<<20, 9))
	cerr := w.Close()
	if werr == nil && cerr == nil {
		t.Fatal("expected the destination's error")
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected an error writing after close")
	}
	if err := w.Close(); err == nil {
		t.Fatal("a second Close should repeat the error")
	}
}

// buildReference builds testdata/pgzipref, the real klauspost/pgzip v1.2.6
// over klauspost/compress v1.11.3, which lives in its own module because
// Go cannot load two versions of klauspost/compress at once.
func buildReference(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "pgzipref")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join("testdata", "pgzipref")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pgzipref: %v: %s", err, out)
	}
	return bin
}

// referencePgzip compresses data with the reference binary and returns the
// whole gzip file.
func referencePgzip(t *testing.T, bin string, data []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdin = bytes.NewReader(data)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("pgzipref %v: %v: %s", args, err, errb.String())
	}
	return out.Bytes()
}

// variant pairs reference flags with the parameters that should reproduce
// them.
type variant struct {
	args []string
	p    engine.DeflateParams
}

func variants() []variant {
	var out []variant
	for l := -2; l <= 9; l++ {
		if l == -1 {
			continue
		}
		out = append(out, variant{[]string{"-level", strconv.Itoa(l)}, engine.DeflateParams{Level: l}})
	}
	for _, l := range []int{1, 5, 9} {
		lvl := strconv.Itoa(l)
		out = append(out,
			variant{[]string{"-level", lvl, "-block", strconv.Itoa(128 << 10)}, engine.DeflateParams{Level: l, BlockSize: 128}},
			variant{[]string{"-level", lvl, "-block", strconv.Itoa(4 << 20)}, engine.DeflateParams{Level: l, BlockSize: 4096}},
			// Neither the number of blocks in flight nor the caller's write
			// size changes pgzip's output.
			variant{[]string{"-level", lvl, "-blocks", "1"}, engine.DeflateParams{Level: l}},
			variant{[]string{"-level", lvl, "-blocks", "16", "-chunk", "4096"}, engine.DeflateParams{Level: l}},
		)
	}
	return out
}

// boundaryInputs are sizes around pgzip's tail and block boundaries.
func boundaryInputs() []fixtures.Fixture {
	sizes := []int{0, 1, 100, 16383, 16384, 16385, 1<<20 - 1, 1 << 20, 1<<20 + 1, 2 << 20, 2<<20 + 1, 3<<20 + 12345}
	var out []fixtures.Fixture
	for _, n := range sizes {
		out = append(out,
			fixtures.Fixture{Name: fmt.Sprintf("text-%d", n), Data: fixtures.Text(n)},
			fixtures.Fixture{Name: fmt.Sprintf("mixed-%d", n), Data: fixtures.Mixed(n)},
		)
	}
	return out
}

func TestMatchesPgzip(t *testing.T) {
	bin := buildReference(t)
	inputs := append(fixtures.All(), boundaryInputs()...)
	for _, f := range inputs {
		for _, v := range variants() {
			t.Run(f.Name+"/"+strings.Join(v.args, " "), func(t *testing.T) {
				file := referencePgzip(t, bin, f.Data, v.args...)
				hdr, err := format.ParseGzipHeader(bufio.NewReader(bytes.NewReader(file)))
				if err != nil {
					t.Fatal(err)
				}
				// pgzip 1.2.6 writes OS 255 and, unless the caller sets a
				// ModTime, the zero time.Time truncated to 32 bits: the
				// signature by which its files can be told apart.
				if hdr.OS != 255 || hdr.ModTime != 0x886e0900 {
					t.Fatalf("header %x: OS %d, mtime %#x", hdr.Raw, hdr.OS, hdr.ModTime)
				}
				want := file[len(hdr.Raw) : len(file)-8]
				got := compress(t, v.p, f.Data, 0)
				if !bytes.Equal(got, want) {
					t.Fatalf("output differs from pgzip %v: got %d bytes, want %d (first difference at %d)", v.args, len(got), len(want), firstDiff(got, want))
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
