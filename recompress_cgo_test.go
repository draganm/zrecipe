//go:build cgo

package zrecipe

import (
	"errors"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/libzstd"
	"github.com/draganm/zrecipe/engine/zlib"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
)

// TestRecompressRoundTripCgo is the cgo half of TestRecompressRoundTrip
// (recompress_test.go), covering the zlib and libzstd engines; it uses the
// checkRoundTrip helper defined there.
func TestRecompressRoundTripCgo(t *testing.T) {
	for _, f := range fixtures.All() {
		files := map[string][]byte{
			"zlib-0":       enginetest.Gzip(t, zlib.New(), engine.DeflateParams{Level: 0, Strategy: "default", WindowBits: 15, MemLevel: 8}, f.Data),
			"zlib-6":       enginetest.Gzip(t, zlib.New(), engine.DeflateParams{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8}, f.Data),
			"libzstd-3":    enginetest.Zstd(t, libzstd.New(), engine.ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true}, f.Data),
			"libzstd-7-mt": enginetest.Zstd(t, libzstd.New(), engine.ZstdParams{Level: 7, Workers: 1}, f.Data),
		}
		for name, file := range files {
			checkRoundTrip(t, f.Name, name, file, f.Data)
		}
	}
}

// TestRecompressInputMismatchZstd proves ErrInputMismatch wins over
// whatever error the zstd engine reports when given the wrong-sized input:
// the engine writer can fail well before EOF (a pledged size mismatch, a
// short write once the encoder validates length), but the real problem is
// the input, and that must be what Recompress reports.
func TestRecompressInputMismatchZstd(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Zstd(t, libzstd.New(), engine.ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true}, data), nil)
	if _, err := recompress(t, p, data[:len(data)-100], nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("short input: %v", err)
	}
	longer := append(append([]byte{}, data...), data[:100]...)
	if _, err := recompress(t, p, longer, nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("long input: %v", err)
	}
}
