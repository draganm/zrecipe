package zrecipe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/draganm/zrecipe/fixtures"
)

func tool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH; run inside the flake dev shell", name)
	}
	return p
}

// run executes a compressor. If stdin is non-nil it is piped; otherwise
// args must name an input file and the output is read from outFile.
func run(t *testing.T, stdin []byte, outFile string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(tool(t, name), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, errb.String())
	}
	if outFile != "" {
		b, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return out.Bytes()
}

func check(t *testing.T, file, data []byte) *Params {
	t.Helper()
	p, err := Analyze(context.Background(), bytes.NewReader(file), nil)
	if err != nil {
		if errors.Is(err, ErrNotReproducible) {
			t.Errorf("NOT REPRODUCIBLE: %v", err)
			return nil
		}
		t.Fatalf("Analyze: %v", err)
	}
	var out bytes.Buffer
	if err := Recompress(context.Background(), p, bytes.NewReader(data), &out, nil); err != nil {
		t.Fatalf("Recompress: %v", err)
	}
	if !bytes.Equal(out.Bytes(), file) {
		t.Fatal("recompressed bytes differ")
	}
	switch p.Format {
	case FormatGzip:
		t.Logf("reproduced by %s %s %+v", p.Engine, p.EngineVersion, p.Gzip.DeflateParams)
	case FormatZstd:
		t.Logf("reproduced by %s %s %+v", p.Engine, p.EngineVersion, *p.Zstd)
	}
	return p
}

// skipWithoutCgoEngines skips a wild-fixture test when built without cgo.
// The pigz and zstd CLI fixtures are reproducible only against the matching
// cgo engine (zlib, libzstd) that shares their implementation, so without
// cgo almost every case is unreproducible and the test would fail for a
// reason unrelated to whatever it's meant to check. GNU gzip fixtures are
// covered by the pure-Go gnu-gzip engine and do not use this.
func skipWithoutCgoEngines(t *testing.T) {
	t.Helper()
	if len(cgoEngines()) == 0 {
		t.Skip("no cgo engines compiled in (CGO_ENABLED=0); this fixture is realistically reproducible only against the matching cgo engine")
	}
}

func wildInputs() []fixtures.Fixture {
	return []fixtures.Fixture{
		{Name: "text-1m", Data: fixtures.Text(1 << 20)},
		// Not a multiple of any block size (deflate 64k, zstd's default
		// 128k), unlike every other fixture here.
		{Name: "text-1000003", Data: fixtures.Text(1000003)},
		{Name: "mixed-3m", Data: fixtures.Mixed(3 << 20)},
		{Name: "tiny", Data: []byte("tiny input\n")},
	}
}

// TestWildGzip needs no cgo: the pure-Go gnu-gzip engine reproduces GNU
// gzip at every level, on every fixture, from a file and from stdin.
func TestWildGzip(t *testing.T) {
	tool(t, "gzip")
	for _, f := range wildInputs() {
		dir := t.TempDir()
		src := filepath.Join(dir, f.Name)
		os.WriteFile(src, f.Data, 0o644)
		for level := 1; level <= 9; level++ {
			lvl := "-" + string(rune('0'+level))
			t.Run(f.Name+"/stdin"+lvl, func(t *testing.T) {
				check(t, run(t, f.Data, "", "gzip", "-c", lvl), f.Data)
			})
			t.Run(f.Name+"/rsyncable"+lvl, func(t *testing.T) {
				check(t, run(t, f.Data, "", "gzip", "-c", "--rsyncable", lvl), f.Data)
			})
			t.Run(f.Name+"/file"+lvl, func(t *testing.T) {
				out := src + ".gz"
				os.Remove(out)
				run(t, nil, out, "gzip", "-k", lvl, src)
				b, _ := os.ReadFile(out)
				check(t, b, f.Data)
			})
		}
	}
}

// TestWildPigz covers both pigz code paths (-p1 and more than one thread),
// --independent, --rsyncable and a non-default block size; all are
// reproduced by the cgo pigz engine.
func TestWildPigz(t *testing.T) {
	skipWithoutCgoEngines(t)
	tool(t, "pigz")
	for _, f := range wildInputs() {
		for _, args := range [][]string{
			{"-1"}, {"-6"}, {"-9"}, {"-0"},
			{"-p1", "-6"}, {"-p4", "-6"}, {"-p1", "-0"},
			{"-i", "-6"}, {"-R", "-6"}, {"-b", "64", "-6"},
			{"-p1", "-i", "-6"}, {"-p1", "-R", "-6"},
		} {
			name := filepath.Join(args...)
			t.Run(f.Name+"/"+name, func(t *testing.T) {
				check(t, run(t, f.Data, "", "pigz", append([]string{"-c"}, args...)...), f.Data)
			})
		}
	}
}

// buildPgzipRef builds engine/pgzip/testdata/pgzipref, the real
// klauspost/pgzip v1.2.6 over klauspost/compress v1.11.3 (its own module,
// since this one links a newer klauspost/compress), and returns its path.
func buildPgzipRef(t *testing.T) string {
	t.Helper()
	tool(t, "go")
	bin := filepath.Join(t.TempDir(), "pgzipref")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join("engine", "pgzip", "testdata", "pgzipref")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pgzipref: %v: %s", err, out)
	}
	return bin
}

// TestWildPgzip needs no cgo: the pure-Go pgzip engine reproduces
// klauspost/pgzip's files at every level, with and without a custom block
// size, whatever the thread count.
func TestWildPgzip(t *testing.T) {
	bin := buildPgzipRef(t)
	for _, f := range wildInputs() {
		for _, args := range [][]string{
			{"-level", "5"}, {"-level", "1"}, {"-level", "9"}, {"-level", "0"}, {"-level", "-2"},
			{"-level", "5", "-blocks", "1"},
			{"-level", "6", "-block", "131072"},
			{"-level", "3", "-block", "4194304"},
		} {
			name := filepath.Join(args...)
			t.Run(f.Name+"/"+name, func(t *testing.T) {
				p := check(t, run(t, f.Data, "", bin, args...), f.Data)
				if p != nil && p.Engine != "pgzip" {
					t.Fatalf("reproduced by %s, want pgzip", p.Engine)
				}
			})
		}
	}
}

func TestWildZstd(t *testing.T) {
	skipWithoutCgoEngines(t)
	tool(t, "zstd")
	for _, f := range wildInputs() {
		dir := t.TempDir()
		src := filepath.Join(dir, f.Name)
		os.WriteFile(src, f.Data, 0o644)
		variants := [][]string{
			{"-1"}, {"-3"}, {"-6"}, {"-9"}, {"-12"}, {"-15"}, {"-19"},
			{"--ultra", "-22"}, {"--fast=3"},
			{"-3", "-T0"}, {"-3", "-T1"}, {"-3", "-T4"}, {"-3", "--single-thread"},
			{"-3", "--no-check"}, {"-3", "--long"}, {"-9", "--long=27"},
		}
		for _, args := range variants {
			name := filepath.Join(args...)
			t.Run(f.Name+"/stdin/"+name, func(t *testing.T) {
				check(t, run(t, f.Data, "", "zstd", append([]string{"-c", "-q"}, args...)...), f.Data)
			})
			t.Run(f.Name+"/file/"+name, func(t *testing.T) {
				out := filepath.Join(dir, "out.zst")
				os.Remove(out)
				run(t, nil, out, "zstd", append([]string{"-q", "-f", "-o", out}, append(args, src)...)...)
				b, _ := os.ReadFile(out)
				check(t, b, f.Data)
			})
		}
	}
}
