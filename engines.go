package zrecipe

import (
	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/gnugzip"
	"github.com/draganm/zrecipe/engine/goflate"
	"github.com/draganm/zrecipe/engine/kpflate"
	"github.com/draganm/zrecipe/engine/kpzstd"
	"github.com/draganm/zrecipe/engine/pgzip"
)

// DefaultEngines returns every engine compiled into the binary, most likely
// producers of files from the wild first: the GNU gzip port, then the cgo
// engines zlib, pigz and libzstd (present only when built with cgo), then
// the remaining pure-Go engines, with the klauspost/pgzip port after the
// klauspost/compress engine whose current version it does not share.
func DefaultEngines() []engine.Engine {
	engines := append([]engine.Engine{gnugzip.New()}, cgoEngines()...)
	return append(engines, goflate.New(), kpflate.New(), pgzip.New(), kpzstd.New())
}
