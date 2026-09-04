package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/draganm/zrecipe"
	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/goflate"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
)

// hasCgoEngines reports whether this binary was built with cgo, and so has
// the zlib and libzstd engines available. The "engines" subcommand's output
// (and therefore what it can be asserted to contain) depends on it.
func hasCgoEngines() bool {
	_, ok := engine.ByName(zrecipe.DefaultEngines(), "zlib")
	return ok
}

func runApp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	app := newApp()
	var out bytes.Buffer
	app.Writer = &out
	app.ErrWriter = &out
	err := app.Run(append([]string{"zrecipe"}, args...))
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
	if !strings.Contains(string(pj), `"engine": "go-flate"`) && !strings.Contains(string(pj), `"engine": "zlib"`) && !strings.Contains(string(pj), `"engine": "gnu-gzip"`) {
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
	if fi, err := os.Stat(rebuilt); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o644 {
		t.Fatalf("rebuilt file mode = %v, want -rw-r--r--", fi.Mode().Perm())
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(matches) > 0 {
		t.Fatalf("temp file left behind: %v", matches)
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

// TestAnalyzeFailureLeavesNoOutputFiles proves that a failing analyze does
// not leave a partial --uncompressed or --params file behind: main.go must
// close and remove both once Analyze (or writing params) errors.
func TestAnalyzeFailureLeavesNoOutputFiles(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(10000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	truncated := file[:len(file)-20]
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, truncated, 0o644)
	out := filepath.Join(dir, "out")
	params := filepath.Join(dir, "p.json")
	if _, err := runApp(t, "analyze", "--uncompressed", out, "--params", params, gz); err == nil {
		t.Fatal("expected failure on truncated gzip")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("--uncompressed file must not exist after failure")
	}
	if _, err := os.Stat(params); err == nil {
		t.Fatal("--params file must not exist after failure")
	}
}

func TestRecompressFailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(10000)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), 0o644)
	params := filepath.Join(dir, "params.json")
	if _, err := runApp(t, "analyze", "--params", params, gz); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	wrong := filepath.Join(dir, "wrong")
	os.WriteFile(wrong, []byte("not the content"), 0o644)
	rebuilt := filepath.Join(dir, "out.gz")
	_, err := runApp(t, "recompress", "--params", params, wrong, rebuilt)
	if err == nil {
		t.Fatal("expected failure")
	}
	// The error must actually be the uncompressed-input mismatch (the
	// ErrInputMismatch text), not merely some other failure that happens to
	// leave no output: this is what makes the test unable to pass by
	// accident.
	if !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected an input-mismatch error, got: %v", err)
	}
	if _, err := os.Stat(rebuilt); err == nil {
		t.Fatal("output must not exist after failure")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(matches) > 0 {
		t.Fatalf("temp file left behind: %v", matches)
	}
}

// TestAnalyzeFlagsFirstSucceeds proves the documented flags-first form
// works: urfave/cli v2 parses flags only up to the first positional
// argument, so --params must precede <file>.
func TestAnalyzeFlagsFirstSucceeds(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(1000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, file, 0o644)
	params := filepath.Join(dir, "params.json")
	if out, err := runApp(t, "analyze", "--params", params, gz); err != nil {
		t.Fatalf("flags-first analyze: %v: %s", err, out)
	}
	if _, err := os.Stat(params); err != nil {
		t.Fatalf("params file not written: %v", err)
	}
}

// TestAnalyzeFlagsLastFails documents the urfave/cli v2 limitation: once a
// positional argument is seen, everything after it (including things that
// look like flags) is treated as a further positional argument, so the
// command's NArg() check rejects it with a usage error instead of silently
// ignoring --params.
func TestAnalyzeFlagsLastFails(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(1000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, file, 0o644)
	params := filepath.Join(dir, "params.json")
	_, err := runApp(t, "analyze", gz, "--params", params)
	if err == nil {
		t.Fatal("expected a usage error when flags follow the positional argument")
	}
	if !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("got %v", err)
	}
}

func TestEngines(t *testing.T) {
	out, err := runApp(t, "engines")
	if err != nil || !strings.Contains(out, "go-flate") || !strings.Contains(out, "klauspost-zstd") {
		t.Fatalf("%q %v", out, err)
	}
	// zlib/libzstd are present only in binaries built with cgo (this test
	// binary included), see engines_cgo.go/engines_nocgo.go.
	if hasCgoEngines() {
		if !strings.Contains(out, "zlib") || !strings.Contains(out, "libzstd") {
			t.Fatalf("%q", out)
		}
	}
}

// TestUsageErrorsReturnWithoutExiting proves that a wrong-arg-count action
// returns a plain error instead of a cli.Exit ExitCoder. urfave/cli v2's
// app.Run handles an ExitCoder by printing it and calling os.Exit directly,
// which would kill this test process before Run ever returns; a plain error
// instead propagates normally, so runApp gets it back here.
func TestUsageErrorsReturnWithoutExiting(t *testing.T) {
	dir := t.TempDir()
	_, err := runApp(t, "detect")
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("detect: %v", err)
	}
	_, err = runApp(t, "analyze")
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("analyze: %v", err)
	}
	_, err = runApp(t, "recompress", "--params", filepath.Join(dir, "does-not-exist.json"), "only-one-arg")
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("recompress: %v", err)
	}
}

// TestBuiltBinaryReportsModuleVersions builds the real zrecipe binary and
// checks its engines output. Under go test, the klauspost engines report
// version "(devel)" because test binaries carry no dependency build info
// (see engine.ModuleVersion); a real build reports the version recorded in
// go.mod.
func TestBuiltBinaryReportsModuleVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	bin := filepath.Join(t.TempDir(), "zrecipe")
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
	// The child build inherits CGO_ENABLED from this process's environment
	// (exec.Command with a nil Env), so zlib/libzstd appear in its "engines"
	// output exactly when they appear in this test binary's own.
	if hasCgoEngines() {
		requireLine("zlib", "1.3.2")
		requireLine("libzstd", "1.5.7")
	}
}
