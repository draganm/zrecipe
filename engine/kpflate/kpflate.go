// Package kpflate is the klauspost/compress deflate engine.
package kpflate

import (
	"errors"
	"io"

	"github.com/klauspost/compress/flate"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

const modulePath = "github.com/klauspost/compress"

// Engine produces raw deflate streams with klauspost/compress/flate.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "klauspost-flate" }
func (*Engine) Version() string       { return engine.ModuleVersion(modulePath) }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns levels 0..9 ordered by the XFL hint, then HuffmanOnly.
// klauspost's DefaultCompression (-1) is level 5 and is not listed twice.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	var tier []engine.DeflateParams
	for _, l := range engine.LevelOrder(h.XFL) {
		tier = append(tier, engine.DeflateParams{Level: l})
	}
	tier = append(tier, engine.DeflateParams{Level: flate.HuffmanOnly})
	return [][]engine.DeflateParams{tier}
}

func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Strategy != "" || p.WindowBits != 0 || p.MemLevel != 0 || p.Rsyncable {
		return nil, errors.New("klauspost-flate: strategy, window_bits, mem_level and rsyncable are not supported")
	}
	return flate.NewWriter(w, p.Level)
}
