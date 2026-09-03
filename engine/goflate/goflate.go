// Package goflate is the Go standard library deflate engine.
package goflate

import (
	"compress/flate"
	"errors"
	"io"
	"runtime"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// Engine produces raw deflate streams with compress/flate.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "go-flate" }
func (*Engine) Version() string       { return runtime.Version() }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns levels 0..9 ordered by the XFL hint, then HuffmanOnly.
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
		return nil, errors.New("go-flate: strategy, window_bits, mem_level and rsyncable are not supported")
	}
	return flate.NewWriter(w, p.Level)
}
