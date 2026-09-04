//go:build cgo && race

package pigz

// raceEnabled reports whether the race detector is on; timing tests skip.
const raceEnabled = true
