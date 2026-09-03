package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
)

// DefaultEngines returns every engine compiled into the binary, most likely
// producers of files from the wild first. The cgo engines zlib and libzstd
// are present only when built with cgo.
func DefaultEngines() []engine.Engine {
	return append(cgoEngines(), goflate.New(), kpflate.New(), kpzstd.New())
}
