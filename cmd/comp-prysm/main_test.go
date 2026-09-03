package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

func runApp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	app := newApp()
	var out bytes.Buffer
	app.Writer = &out
	app.ErrWriter = &out
	err := app.Run(append([]string{"comp-prysm"}, args...))
	return out.String(), err
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "a.gz")
	os.WriteFile(gz, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, []byte("x")), 0o644)
	out, err := runApp(t, "detect", gz)
	if err != nil || strings.TrimSpace(out) != "gzip" {
		t.Fatalf("%q %v", out, err)
	}
	plain := filepath.Join(dir, "a.txt")
	os.WriteFile(plain, []byte("x"), 0o644)
	out, _ = runApp(t, "detect", plain)
	if strings.TrimSpace(out) != "none" {
		t.Fatalf("%q", out)
	}
}

func TestAnalyzeThenRecompress(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Mixed(200 << 10)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, file, 0o644)
	params := filepath.Join(dir, "params.json")
	unc := filepath.Join(dir, "in")
	if out, err := runApp(t, "analyze", "--params", params, "--uncompressed", unc, gz); err != nil {
		t.Fatalf("analyze: %v: %s", err, out)
	}
	got, _ := os.ReadFile(unc)
	if !bytes.Equal(got, data) {
		t.Fatal("uncompressed output differs")
	}
	pj, _ := os.ReadFile(params)
	if !strings.Contains(string(pj), `"engine": "go-flate"`) && !strings.Contains(string(pj), `"engine": "zlib"`) {
		t.Fatalf("params: %s", pj)
	}
	rebuilt := filepath.Join(dir, "out.gz")
	if out, err := runApp(t, "recompress", "--params", params, unc, rebuilt); err != nil {
		t.Fatalf("recompress: %v: %s", err, out)
	}
	back, _ := os.ReadFile(rebuilt)
	if !bytes.Equal(back, file) {
		t.Fatal("rebuilt file differs")
	}
	if _, err := os.Stat(rebuilt + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestAnalyzeParamsToStdout(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "a.txt")
	os.WriteFile(plain, []byte("hello"), 0o644)
	out, err := runApp(t, "analyze", plain)
	if err != nil || !strings.Contains(out, `"format": "none"`) {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRecompressFailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(10000)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), 0o644)
	params := filepath.Join(dir, "params.json")
	runApp(t, "analyze", "--params", params, gz)
	wrong := filepath.Join(dir, "wrong")
	os.WriteFile(wrong, []byte("not the content"), 0o644)
	rebuilt := filepath.Join(dir, "out.gz")
	if _, err := runApp(t, "recompress", "--params", params, wrong, rebuilt); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(rebuilt); err == nil {
		t.Fatal("output must not exist after failure")
	}
}

func TestEngines(t *testing.T) {
	out, err := runApp(t, "engines")
	if err != nil || !strings.Contains(out, "zlib") || !strings.Contains(out, "libzstd") {
		t.Fatalf("%q %v", out, err)
	}
}

// TestBuiltBinaryReportsModuleVersions builds the real comp-prysm binary and
// checks its engines output. Under go test, the klauspost engines report
// version "(devel)" because test binaries carry no dependency build info
// (see engine.ModuleVersion); a real build reports the version recorded in
// go.mod.
func TestBuiltBinaryReportsModuleVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "comp-prysm")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	defer os.Remove(bin)

	out, err := exec.Command(bin, "engines").CombinedOutput()
	if err != nil {
		t.Fatalf("engines: %v: %s", err, out)
	}
	lines := strings.Split(string(out), "\n")
	requireLine := func(substrs ...string) {
		t.Helper()
		for _, line := range lines {
			all := true
			for _, s := range substrs {
				if !strings.Contains(line, s) {
					all = false
					break
				}
			}
			if all {
				return
			}
		}
		t.Fatalf("no output line contains %v:\n%s", substrs, out)
	}
	requireLine("klauspost-flate", "v1.20.0")
	requireLine("klauspost-zstd", "v1.20.0")
	requireLine("zlib", "1.3.2")
	requireLine("libzstd", "1.5.7")
}
