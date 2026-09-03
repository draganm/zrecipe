//go:build cgo

package compprysm

import (
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/zlib"
	"github.com/draganm/comp-prysm/fixtures"
)

// TestAnalyzeGzipFindsProducerCgo is the cgo half of TestAnalyzeGzipFindsProducer
// (analyze_test.go), covering the zlib engine; it uses the gzipProducerCase
// type and checkGzipFindsProducer helper defined there.
func TestAnalyzeGzipFindsProducerCgo(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []gzipProducerCase{
		{zlib.New(), engine.DeflateParams{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8}},
		{zlib.New(), engine.DeflateParams{Level: 9, Strategy: "default", WindowBits: 15, MemLevel: 8}},
		{zlib.New(), engine.DeflateParams{Level: 4, Strategy: "filtered", WindowBits: 15, MemLevel: 8}},
	} {
		checkGzipFindsProducer(t, data, tc)
	}
}

// TestAnalyzeZstdFindsProducerCgo is the cgo half of TestAnalyzeZstdFindsProducer
// (analyze_test.go), covering the libzstd engine; it uses the
// zstdProducerCase type and checkZstdFindsProducer helper defined there.
func TestAnalyzeZstdFindsProducerCgo(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []zstdProducerCase{
		{libzstd.New(), engine.ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true}},
		{libzstd.New(), engine.ZstdParams{Level: 12, Workers: 1}},
	} {
		checkZstdFindsProducer(t, data, tc)
	}
}
