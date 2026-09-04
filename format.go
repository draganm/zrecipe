// Package zrecipe makes compressed files reproducible from their
// uncompressed content: Analyze finds the engine and parameters that
// re-create a gzip or zstd file exactly, and Recompress rebuilds it.
package zrecipe

import (
	"io"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
)

// Format identifies a compression container.
type Format = format.Format

const (
	FormatNone = format.None
	FormatGzip = format.Gzip
	FormatZstd = format.Zstd
)

// Parameter types are defined in package engine and re-exported here.
type (
	GzipParams    = engine.GzipParams
	ZstdParams    = engine.ZstdParams
	DeflateParams = engine.DeflateParams
)

// Detect reads the magic bytes and seeks r back to its start. An input
// shorter than any magic sequence is FormatNone.
func Detect(r io.ReadSeeker) (Format, error) { return format.Detect(r) }
