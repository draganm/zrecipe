package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/gnugzip"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
)

// DefaultEngines returns every engine compiled into the binary, most likely
// producers of files from the wild first: the GNU gzip port, then the cgo
// engines zlib and libzstd (present only when built with cgo), then the
// remaining pure-Go engines.
func DefaultEngines() []engine.Engine {
	engines := append([]engine.Engine{gnugzip.New()}, cgoEngines()...)
	return append(engines, goflate.New(), kpflate.New(), kpzstd.New())
}
