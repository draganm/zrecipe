//go:build !cgo

package zrecipe

import "github.com/draganm/zrecipe/engine"

func cgoEngines() []engine.Engine { return nil }
