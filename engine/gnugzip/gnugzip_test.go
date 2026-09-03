package gnugzip

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

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "gnu-gzip" || e.Version() != "1.14" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s %s", e.Name(), e.Version(), e.Format())
	}
}

func TestCandidatesFollowXFL(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 0)
	if len(tiers) != 2 || len(tiers[0]) != 9 || len(tiers[1]) != 9 {
		t.Fatalf("%+v", tiers)
	}
	if tiers[0][0] != (engine.DeflateParams{Level: 9}) || tiers[0][8] != (engine.DeflateParams{Level: 1}) {
		t.Fatalf("tier 1: %+v", tiers[0])
	}
	if tiers[1][0] != (engine.DeflateParams{Level: 9, Rsyncable: true}) {
		t.Fatalf("tier 2: %+v", tiers[1])
	}
	for _, tier := range tiers {
		for _, p := range tier {
			if p.Level == 0 {
				t.Fatalf("level 0 offered: %+v", tiers)
			}
		}
	}
}

func TestRejectsUnsupportedParams(t *testing.T) {
	for _, p := range []engine.DeflateParams{
		{Level: 0},
		{Level: 10},
		{Level: 6, Strategy: engine.StrategyFiltered},
		{Level: 6, WindowBits: 15},
		{Level: 6, MemLevel: 8},
	} {
		if _, err := New().NewWriter(io.Discard, p); err == nil {
			t.Errorf("%+v: expected an error", p)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}

// compress runs the engine over data, feeding it in chunks of the given
// size (0 means one Write).
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
	data := fixtures.Mixed(200<<10 + 7)
	for _, level := range []int{1, 6, 9} {
		p := engine.DeflateParams{Level: level}
		want := compress(t, p, data, 0)
		for _, chunk := range []int{1, 7, 4096, 65535, 65536, 65537, 100000} {
			if got := compress(t, p, data, chunk); !bytes.Equal(got, want) {
				t.Errorf("level %d: chunk %d differs from a single write", level, chunk)
			}
		}
	}
}

func TestOutputInflates(t *testing.T) {
	for _, f := range fixtures.All() {
		for level := 1; level <= 9; level++ {
			for _, rs := range []bool{false, true} {
				out := compress(t, engine.DeflateParams{Level: level, Rsyncable: rs}, f.Data, 0)
				got, err := io.ReadAll(flate.NewReader(bytes.NewReader(out)))
				if err != nil {
					t.Fatalf("%s level %d rsync %v: inflate: %v", f.Name, level, rs, err)
				}
				if !bytes.Equal(got, f.Data) {
					t.Fatalf("%s level %d rsync %v: inflated content differs", f.Name, level, rs)
				}
			}
		}
	}
}

func TestWriteAfterClose(t *testing.T) {
	w, _ := New().NewWriter(io.Discard, engine.DeflateParams{Level: 6})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("expected an error")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
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
}

// gnuGzip returns the path of a GNU gzip on PATH, or skips.
func gnuGzip(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("gzip")
	if err != nil {
		t.Skip("gzip not on PATH; run inside the flake dev shell")
	}
	out, err := exec.Command(p, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "Free Software Foundation") {
		t.Skipf("%s is not GNU gzip: %s", p, out)
	}
	return p
}

// referenceGzip compresses a file with the real gzip and returns the raw
// deflate payload.
func referenceGzip(t *testing.T, gzip, file string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(gzip, append(append([]string{"-c", "-n"}, args...), file)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("gzip %v: %v: %s", args, err, errb.String())
	}
	b := out.Bytes()
	hdr, err := format.ParseGzipHeader(bufio.NewReader(bytes.NewReader(b)))
	if err != nil {
		t.Fatal(err)
	}
	return b[len(hdr.Raw) : len(b)-8]
}

// boundaryInputs are sizes around the points where GNU gzip's window
// slides (64 KiB, then every 32 KiB) and where its end-of-input handling
// differs: inputs whose last bytes land past windowSize-minLookahead after
// the final slide are emitted without matching.
func boundaryInputs() []fixtures.Fixture {
	sizes := []int{0, 1, 2, 3, 4, 100, 261, 262, 263, 4095, 4096, 4097, 32767, 32768,
		65273, 65274, 65275, 65535, 65536, 65537, 98204, 98303, 98304, 98305,
		131072, 131500, 200000}
	var out []fixtures.Fixture
	for _, n := range sizes {
		out = append(out,
			fixtures.Fixture{Name: fmt.Sprintf("text-%d", n), Data: fixtures.Text(n)},
			fixtures.Fixture{Name: fmt.Sprintf("zeros-%d", n), Data: fixtures.Zeros(n)},
			fixtures.Fixture{Name: fmt.Sprintf("mixed-%d", n), Data: fixtures.Mixed(n)},
			fixtures.Fixture{Name: fmt.Sprintf("random-%d", n), Data: fixtures.Random(n, int64(n))},
		)
	}
	return out
}

func TestMatchesGNUGzip(t *testing.T) {
	gzip := gnuGzip(t)
	dir := t.TempDir()
	inputs := append(fixtures.All(), boundaryInputs()...)
	for _, f := range inputs {
		src := filepath.Join(dir, f.Name)
		if err := os.WriteFile(src, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
		for level := 1; level <= 9; level++ {
			lvl := fmt.Sprintf("-%d", level)
			t.Run(f.Name+"/"+lvl, func(t *testing.T) {
				want := referenceGzip(t, gzip, src, lvl)
				got := compress(t, engine.DeflateParams{Level: level}, f.Data, 0)
				if !bytes.Equal(got, want) {
					t.Fatalf("output differs from gzip %s: got %d bytes, want %d (first difference at %d)", lvl, len(got), len(want), firstDiff(got, want))
				}
			})
			t.Run(f.Name+"/"+lvl+"/rsyncable", func(t *testing.T) {
				want := referenceGzip(t, gzip, src, lvl, "--rsyncable")
				got := compress(t, engine.DeflateParams{Level: level, Rsyncable: true}, f.Data, 0)
				if !bytes.Equal(got, want) {
					t.Fatalf("output differs from gzip %s --rsyncable: got %d bytes, want %d (first difference at %d)", lvl, len(got), len(want), firstDiff(got, want))
				}
			})
		}
	}
}

func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
