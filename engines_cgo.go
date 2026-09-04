//go:build cgo

package zrecipe

import (
	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/libzstd"
	"github.com/draganm/zrecipe/engine/pigz"
	"github.com/draganm/zrecipe/engine/zlib"
)

// cgoEngines returns the engines that link C libraries: zlib before pigz,
// since for input that fits one pigz block the two produce identical bytes
// and zlib is the simpler record; then libzstd.
func cgoEngines() []engine.Engine {
	return []engine.Engine{zlib.New(), pigz.New(), libzstd.New()}
}
