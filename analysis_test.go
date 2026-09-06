package zrecipe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/goflate"
	"github.com/draganm/zrecipe/engine/kpzstd"
	"github.com/draganm/zrecipe/engine/pgzip"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/search"
)

// startConfirm runs Start and Confirm with a buffer as the tee and returns
// the Params, the Analysis (closed) and what the tee received.
func startConfirm(t *testing.T, file []byte, opts *Options) (*Params, *Analysis, []byte) {
	t.Helper()
	a, err := Start(context.Background(), bytes.NewReader(file), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Close()
	var tee bytes.Buffer
	p, err := a.Confirm(context.Background(), &tee)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	return p, a, tee.Bytes()
}

func TestStartConfirmGzipProducers(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []gzipProducerCase{
		{goflate.New(), engine.DeflateParams{Level: 6}},
		{goflate.New(), engine.DeflateParams{Level: 1}},
		{pgzip.New(), engine.DeflateParams{Level: 5}},
	} {
		file := enginetest.Gzip(t, tc.engine, tc.params, data)
		p, a, got := startConfirm(t, file, nil)
		if a.Format() != FormatGzip || !bytes.Equal(got, data) {
			t.Fatalf("%s %+v: format %s, tee %d bytes", tc.engine.Name(), tc.params, a.Format(), len(got))
		}
		if a.Uncompressed().Size != int64(len(data)) || a.Compressed().Size != int64(len(file)) {
			t.Fatalf("digests %+v %+v", a.Uncompressed(), a.Compressed())
		}
		if want := analyze(t, file, nil); !reflect.DeepEqual(p, want) {
			t.Fatalf("Params differ:\n%+v\n%+v", p, want)
		}
	}
}

func TestStartConfirmZstdProducers(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []zstdProducerCase{
		{kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}},
		{kpzstd.New(), engine.ZstdParams{Level: 3, ContentSize: true, PledgedSize: true, SingleSegment: true}},
	} {
		file := enginetest.Zstd(t, tc.engine, tc.params, data)
		p, a, got := startConfirm(t, file, nil)
		if a.Format() != FormatZstd || !bytes.Equal(got, data) {
			t.Fatalf("%+v: format %s, tee %d bytes", tc.params, a.Format(), len(got))
		}
		if want := analyze(t, file, nil); !reflect.DeepEqual(p, want) {
			t.Fatalf("Params differ:\n%+v\n%+v", p, want)
		}
	}
}

// TestStartConfirmLargeInput reproduces a 5 MiB layer and streams every
// byte of its content to the tee.
func TestStartConfirmLargeInput(t *testing.T) {
	data := fixtures.Mixed(5 << 20)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	p, _, got := startConfirm(t, file, &Options{Parallelism: 4})
	if p.Engine != "go-flate" || p.Gzip.Level != 6 || !bytes.Equal(got, data) {
		t.Fatalf("%+v, tee %d bytes", p, len(got))
	}
}

func TestStartNone(t *testing.T) {
	data := fixtures.Text(50 << 10)
	p, a, got := startConfirm(t, data, nil)
	if a.Format() != FormatNone || !a.Verified() || !bytes.Equal(got, data) {
		t.Fatalf("format %s verified %v tee %d", a.Format(), a.Verified(), len(got))
	}
	if p.Format != FormatNone || p.Compressed != p.Uncompressed || p.Uncompressed.Size != int64(len(data)) || p.Engine != "" {
		t.Fatalf("%+v", p)
	}
}

func TestStartNotReproducibleWritesNothing(t *testing.T) {
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, fixtures.Text(100<<10))
	var unc bytes.Buffer
	_, err := Analyze(context.Background(), bytes.NewReader(file), &Options{Engines: []engine.Engine{kpzstd.New()}, Uncompressed: &unc})
	if !errors.Is(err, ErrNotReproducible) || unc.Len() != 0 {
		t.Fatalf("err %v, %d bytes written", err, unc.Len())
	}
}

// TestConfirmDivergingCandidate replaces the settled candidate's level and
// checks Confirm catches the divergence without streaming everything.
func TestConfirmDivergingCandidate(t *testing.T) {
	data := fixtures.Text(6 << 20)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	a, err := Start(context.Background(), bytes.NewReader(file), &Options{Parallelism: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.params.Engine != "go-flate" || a.params.Gzip.Level != 6 {
		t.Fatalf("settled on %s %+v", a.params.Engine, a.params.Gzip.DeflateParams)
	}
	a.params.Gzip.Level = 9
	var tee bytes.Buffer
	_, err = a.Confirm(context.Background(), &tee)
	if !errors.Is(err, ErrNotReproducible) || !errors.Is(err, search.ErrMismatch) {
		t.Fatalf("got %v", err)
	}
	if tee.Len() >= len(data) {
		t.Fatalf("tee received the whole content (%d bytes) after a divergence", tee.Len())
	}
}

type failingTee struct {
	after int
	n     int
	err   error
}

func (f *failingTee) Write(p []byte) (int, error) {
	f.n += len(p)
	if f.n > f.after {
		return 0, f.err
	}
	return len(p), nil
}

func TestConfirmTeeErrorIsReturned(t *testing.T) {
	boom := errors.New("tee is full")
	data := fixtures.Text(2 << 20)
	for _, file := range [][]byte{enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), data} {
		a, err := Start(context.Background(), bytes.NewReader(file), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.Confirm(context.Background(), &failingTee{after: 100 << 10, err: boom})
		a.Close()
		if !errors.Is(err, boom) || errors.Is(err, ErrNotReproducible) {
			t.Fatalf("format %s: got %v", a.Format(), err)
		}
	}
}

func TestConfirmCancelled(t *testing.T) {
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, fixtures.Text(1<<20))
	a, err := Start(context.Background(), bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Confirm(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestConfirmOnceAndNotAfterClose(t *testing.T) {
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, fixtures.Text(10<<10))
	a, err := Start(context.Background(), bytes.NewReader(file), &Options{MaxInMemory: 1, TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Confirm(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Confirm(context.Background(), nil); err == nil {
		t.Fatal("second Confirm succeeded")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Start(context.Background(), bytes.NewReader(file), nil)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	if _, err := b.Confirm(context.Background(), nil); err == nil {
		t.Fatal("Confirm after Close succeeded")
	}
}
