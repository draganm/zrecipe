//go:build cgo

package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/pigz"
	"github.com/draganm/comp-prysm/engine/zlib"
)

// cgoEngines returns the engines that link C libraries: zlib before pigz,
// since for input that fits one pigz block the two produce identical bytes
// and zlib is the simpler record; then libzstd.
func cgoEngines() []engine.Engine {
	return []engine.Engine{zlib.New(), pigz.New(), libzstd.New()}
}
