//go:build !cgo

package compprysm

import "github.com/draganm/comp-prysm/engine"

func cgoEngines() []engine.Engine { return nil }
