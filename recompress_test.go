package compprysm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

func recompress(t *testing.T, p *Params, data []byte, opts *RecompressOptions) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := Recompress(context.Background(), p, bytes.NewReader(data), &out, opts)
	return out.Bytes(), err
}

// checkRoundTrip is shared with recompress_cgo_test.go's
// TestRecompressRoundTripCgo, which runs the same check for the cgo-only
// zlib and libzstd engines.
func checkRoundTrip(t *testing.T, fixtureName, caseName string, file, data []byte) {
	t.Helper()
	t.Run(fixtureName+"/"+caseName, func(t *testing.T) {
		p := analyze(t, file, nil)
		out, err := recompress(t, p, data, nil)
		if err != nil {
			t.Fatalf("recompress: %v", err)
		}
		if !bytes.Equal(out, file) {
			t.Fatal("recompressed bytes differ")
		}
	})
}

func TestRecompressRoundTrip(t *testing.T) {
	for _, f := range fixtures.All() {
		files := map[string][]byte{
			"goflate-9":     enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 9}, f.Data),
			"kpzstd-stream": enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, f.Data),
			"none":          f.Data,
		}
		for name, file := range files {
			checkRoundTrip(t, f.Name, name, file, f.Data)
		}
	}
}

func TestRecompressInputMismatch(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	wrong := append([]byte{}, data...)
	wrong[500] ^= 1
	if _, err := recompress(t, p, wrong, nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, err := recompress(t, p, data[:9000], nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("short input: %v", err)
	}
}

func TestRecompressDigestMismatch(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	p.Gzip.Level = 1
	if _, err := recompress(t, p, data, nil); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestRecompressEngineChecks(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)

	q := *p
	q.Engine = "nope"
	if _, err := recompress(t, &q, data, nil); !errors.Is(err, ErrEngineUnavailable) {
		t.Fatalf("unknown engine: %v", err)
	}

	q = *p
	q.EngineVersion = "go0.0"
	if _, err := recompress(t, &q, data, nil); !errors.Is(err, ErrEngineVersionMismatch) {
		t.Fatalf("version: %v", err)
	}
	out, err := recompress(t, &q, data, &RecompressOptions{AllowVersionMismatch: true})
	if err != nil || len(out) == 0 {
		t.Fatalf("allow mismatch: %v", err)
	}

	q = *p
	q.Engine = "klauspost-zstd"
	if _, err := recompress(t, &q, data, &RecompressOptions{AllowVersionMismatch: true}); !errors.Is(err, ErrEngineUnavailable) {
		t.Fatalf("wrong format engine: %v", err)
	}
}

func TestRecompressInvalidParams(t *testing.T) {
	p := &Params{Version: ParamsVersion, Format: FormatGzip}
	if _, err := recompress(t, p, nil, nil); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("got %v", err)
	}
	p = &Params{Version: 7, Format: FormatNone}
	if _, err := recompress(t, p, nil, nil); !errors.Is(err, ErrParamsVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestRecompressCancelled(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Recompress(ctx, p, bytes.NewReader(data), io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
