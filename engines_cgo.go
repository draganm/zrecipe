//go:build cgo

package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/zlib"
)

func cgoEngines() []engine.Engine {
	return []engine.Engine{zlib.New(), libzstd.New()}
}
