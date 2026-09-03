package compprysm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

// seekOnly hides io.ReaderAt so the sequential search path is exercised.
// The reader is a named field, not embedded, so ReadAt is not promoted.
type seekOnly struct{ r *bytes.Reader }

func (s seekOnly) Read(p []byte) (int, error)                 { return s.r.Read(p) }
func (s seekOnly) Seek(off int64, whence int) (int64, error) { return s.r.Seek(off, whence) }

func analyze(t *testing.T, file []byte, opts *Options) *Params {
	t.Helper()
	p, err := Analyze(context.Background(), bytes.NewReader(file), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return p
}

func TestAnalyzeNone(t *testing.T) {
	data := fixtures.Text(1000)
	var unc bytes.Buffer
	p := analyze(t, data, &Options{Uncompressed: &unc})
	if p.Format != FormatNone || p.Engine != "" || p.Compressed != p.Uncompressed || p.Compressed.Size != 1000 {
		t.Fatalf("%+v", p)
	}
	if !bytes.Equal(unc.Bytes(), data) {
		t.Fatal("uncompressed writer did not receive the content")
	}
	if p.Version != ParamsVersion {
		t.Fatalf("version %d", p.Version)
	}
}

func TestAnalyzeEmptyInput(t *testing.T) {
	p := analyze(t, nil, nil)
	if p.Format != FormatNone || p.Compressed.Size != 0 {
		t.Fatalf("%+v", p)
	}
}

// gzipProducerCase and checkGzipFindsProducer are shared with
// analyze_cgo_test.go, which runs the same check against the cgo-only zlib
// engine; this file only compiles against the pure-Go engines so the
// no-cgo build stays buildable.
type gzipProducerCase struct {
	engine engine.DeflateEngine
	params engine.DeflateParams
}

func checkGzipFindsProducer(t *testing.T, data []byte, tc gzipProducerCase) {
	t.Helper()
	file := enginetest.Gzip(t, tc.engine, tc.params, data)
	var unc bytes.Buffer
	p := analyze(t, file, &Options{Uncompressed: &unc})
	if p.Format != FormatGzip || p.Gzip == nil {
		t.Fatalf("%+v", p)
	}
	if !bytes.Equal(unc.Bytes(), data) || p.Uncompressed.Size != int64(len(data)) || p.Compressed.Size != int64(len(file)) {
		t.Fatalf("sizes/content wrong: %+v", p)
	}
	hdr, _ := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64)
	if !bytes.Equal(hdr, file[:10]) {
		t.Fatalf("header %x", hdr)
	}
	// The found candidate must reproduce the file, whichever engine it names.
	e, _ := engine.ByName(DefaultEngines(), p.Engine)
	again := enginetest.Gzip(t, e.(engine.DeflateEngine), p.Gzip.DeflateParams, data)
	if !bytes.Equal(again, file) {
		t.Fatalf("%s %+v does not reproduce a file made by %s %+v", p.Engine, p.Gzip.DeflateParams, tc.engine.Name(), tc.params)
	}
	t.Logf("%s %+v -> %s %s %+v", tc.engine.Name(), tc.params, p.Engine, p.EngineVersion, p.Gzip.DeflateParams)
}

func TestAnalyzeGzipFindsProducer(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []gzipProducerCase{
		{goflate.New(), engine.DeflateParams{Level: 6}},
		{goflate.New(), engine.DeflateParams{Level: 1}},
	} {
		checkGzipFindsProducer(t, data, tc)
	}
}

// zstdProducerCase and checkZstdFindsProducer are shared with
// analyze_cgo_test.go; see the comment on gzipProducerCase above.
type zstdProducerCase struct {
	engine engine.ZstdEngine
	params engine.ZstdParams
}

func checkZstdFindsProducer(t *testing.T, data []byte, tc zstdProducerCase) {
	t.Helper()
	file := enginetest.Zstd(t, tc.engine, tc.params, data)
	p := analyze(t, file, nil)
	if p.Format != FormatZstd || p.Zstd == nil {
		t.Fatalf("%+v", p)
	}
	e, _ := engine.ByName(DefaultEngines(), p.Engine)
	again := enginetest.Zstd(t, e.(engine.ZstdEngine), *p.Zstd, data)
	if !bytes.Equal(again, file) {
		t.Fatalf("%s %+v does not reproduce a file made by %s %+v", p.Engine, *p.Zstd, tc.engine.Name(), tc.params)
	}
	t.Logf("%s %+v -> %s %s %+v", tc.engine.Name(), tc.params, p.Engine, p.EngineVersion, *p.Zstd)
}

func TestAnalyzeZstdFindsProducer(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []zstdProducerCase{
		{kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}},
		{kpzstd.New(), engine.ZstdParams{Level: 3, ContentSize: true, PledgedSize: true, SingleSegment: true}},
	} {
		checkZstdFindsProducer(t, data, tc)
	}
}

// TestAnalyzeSequentialAndSpilled uses goflate (not zlib) so this file
// compiles without cgo; the sequential/spilled search path it exercises is
// engine-agnostic.
func TestAnalyzeSequentialAndSpilled(t *testing.T) {
	data := fixtures.Text(200 << 10)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	p, err := Analyze(context.Background(), seekOnly{r: bytes.NewReader(file)}, &Options{MaxInMemory: 1, TempDir: t.TempDir(), Parallelism: 8})
	if err != nil {
		t.Fatal(err)
	}
	if p.Gzip == nil || p.Gzip.Level != 6 {
		t.Fatalf("%+v", p)
	}
}

func TestAnalyzeUnsupported(t *testing.T) {
	data := fixtures.Text(1000)
	one := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	multi := append(append([]byte{}, one...), one...)
	if _, err := Analyze(context.Background(), bytes.NewReader(multi), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("multi-member: %v", err)
	}
	z := enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, data)
	multiZ := append(append([]byte{}, z...), z...)
	if _, err := Analyze(context.Background(), bytes.NewReader(multiZ), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("multi-frame: %v", err)
	}
	dict := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x02, 0x40, 0x34, 0x12, 0x01, 0x00, 0x00}
	if _, err := Analyze(context.Background(), bytes.NewReader(dict), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("dictionary: %v", err)
	}
	skip := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	if _, err := Analyze(context.Background(), bytes.NewReader(skip), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("skippable: %v", err)
	}
}

func TestAnalyzeCorrupt(t *testing.T) {
	data := fixtures.Text(10000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	truncated := file[:len(file)-20]
	if _, err := Analyze(context.Background(), bytes.NewReader(truncated), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated: %v", err)
	}
	badTrailer := append([]byte{}, file...)
	badTrailer[len(badTrailer)-1] ^= 0xff
	if _, err := Analyze(context.Background(), bytes.NewReader(badTrailer), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad trailer: %v", err)
	}
	z := enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, data)
	z[len(z)-1] ^= 0xff
	if _, err := Analyze(context.Background(), bytes.NewReader(z), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad zstd checksum: %v", err)
	}
}

func TestAnalyzeNotReproducible(t *testing.T) {
	data := fixtures.Text(10000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	// Only zstd engines offered: no deflate candidates at all.
	_, err := Analyze(context.Background(), bytes.NewReader(file), &Options{Engines: []engine.Engine{kpzstd.New()}})
	if !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("got %v", err)
	}
}

func TestAnalyzeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, fixtures.Text(10000))
	if _, err := Analyze(ctx, bytes.NewReader(file), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

var _ io.ReadSeeker = seekOnly{}
