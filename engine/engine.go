// Package engine defines the compression engines that zrecipe searches
// over, and the parameter types recorded in Params.
package engine

import (
	"io"
	"runtime/debug"

	"github.com/draganm/zrecipe/format"
)

// Format is re-exported from package format.
type Format = format.Format

const (
	FormatNone = format.None
	FormatGzip = format.Gzip
	FormatZstd = format.Zstd
)

// Engine is a compression implementation with a stable name and a version
// that determines its output.
type Engine interface {
	// Name is the stable identifier stored in Params, for example "zlib".
	Name() string
	// Version identifies the implementation build, for example "1.3.2".
	Version() string
	// Format is the container format this engine produces payloads for.
	Format() Format
}

// zlib strategy names.
const (
	StrategyDefault     = "default"
	StrategyFiltered    = "filtered"
	StrategyHuffmanOnly = "huffman_only"
	StrategyRLE         = "rle"
	StrategyFixed       = "fixed"
)

// DeflateParams configures a raw deflate stream.
type DeflateParams struct {
	Level      int    `json:"level"`
	Strategy   string `json:"strategy,omitempty"`    // zlib and pigz (pigz: huffman_only and rle only)
	WindowBits int    `json:"window_bits,omitempty"` // zlib only: 9..15
	MemLevel   int    `json:"mem_level,omitempty"`   // zlib only: 1..9
	Rsyncable  bool   `json:"rsyncable,omitempty"`   // gnu-gzip and pigz: --rsyncable

	// pigz and pgzip: the block the input is cut into, in KiB; 0 means the
	// engine's default (pigz -b 128, pgzip 1024).
	BlockSize int `json:"block_size,omitempty"`

	// pigz only.
	Independent  bool `json:"independent,omitempty"`   // -i: blocks are compressed without the preceding history
	SingleThread bool `json:"single_thread,omitempty"` // -p 1: pigz's single-thread code path, which flushes differently
}

// GzipParams is DeflateParams plus the verbatim gzip header.
type GzipParams struct {
	HeaderB64 string `json:"header_b64"`
	DeflateParams
}

// DeflateEngine produces raw deflate streams (no gzip header or trailer).
type DeflateEngine interface {
	Engine
	// Candidates returns parameter sets grouped by tier, most likely first.
	Candidates(h *format.GzipHeader, uncompressedSize int64) [][]DeflateParams
	// NewWriter returns a writer that compresses into w. Close flushes.
	NewWriter(w io.Writer, p DeflateParams) (io.WriteCloser, error)
}

// ZstdParams configures a zstd frame.
type ZstdParams struct {
	Level         int  `json:"level"`
	WindowLog     int  `json:"window_log,omitempty"` // 0 means the engine default
	Checksum      bool `json:"checksum"`
	ContentSize   bool `json:"content_size"`   // frame header carries the content size
	PledgedSize   bool `json:"pledged_size"`   // encoder knew the source size
	SingleSegment bool `json:"single_segment"` // frame has no window descriptor
	Workers       int  `json:"workers"`        // 0: single-thread path, 1: job-based path
	Long          bool `json:"long,omitempty"` // long distance matching
	// EndWithData records that the producer passed its final input chunk
	// together with the end directive (known-size producers such as the
	// zstd CLI reading a file). When false, end was signalled after all
	// input, which makes libzstd emit an empty last block. Single-thread
	// path only.
	EndWithData bool `json:"end_with_data,omitempty"`
	// EncodeAll records that the klauspost-zstd engine used its one-shot
	// EncodeAll encoding path (which buffers the whole input and encodes it
	// in a single call) rather than its streaming writer.
	EncodeAll bool `json:"encode_all,omitempty"`
	// Head is the number of bytes the klauspost-zstd engine writes before
	// it flushes the encoder once and streams the rest: the shape of a
	// producer that wrote a prefix with Write and handed the remainder to
	// Encoder.ReadFrom, which flushes what Write buffered as a block of
	// its own. containers/image (skopeo, podman, buildah) does this with
	// the 8 bytes it peeked at to detect the source compression.
	Head int `json:"head,omitempty"`
}

// ZstdEngine produces complete zstd frames.
type ZstdEngine interface {
	Engine
	Candidates(h *format.ZstdFrameHeader, uncompressedSize int64) [][]ZstdParams
	NewWriter(w io.Writer, p ZstdParams, uncompressedSize int64) (io.WriteCloser, error)
}

// LevelOrder returns deflate levels 0..9 ordered by likelihood given the gzip
// XFL byte: 2 marks maximum compression, 4 marks fastest.
func LevelOrder(xfl byte) []int {
	switch xfl {
	case 2:
		return []int{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}
	case 4:
		return []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 0}
	default:
		return []int{6, 5, 7, 4, 8, 3, 9, 2, 1, 0}
	}
}

// ModuleVersion returns the version of a dependency module recorded in the
// binary's build info, or "(devel)" when unknown.
func ModuleVersion(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	for _, d := range bi.Deps {
		if d.Path != path {
			continue
		}
		v := d.Version
		if d.Replace != nil {
			v = d.Replace.Version
		}
		if v == "" {
			return "(devel)"
		}
		return v
	}
	return "(devel)"
}

// ByName finds an engine by name.
func ByName(engines []Engine, name string) (Engine, bool) {
	for _, e := range engines {
		if e.Name() == name {
			return e, true
		}
	}
	return nil, false
}
