package compprysm

import "testing"

func TestDefaultEnginesOrderAndNames(t *testing.T) {
	var names []string
	for _, e := range DefaultEngines() {
		names = append(names, e.Name())
		if e.Version() == "" {
			t.Errorf("%s has empty version", e.Name())
		}
	}
	// want is cgoEngines()'s names (zlib, libzstd when built with cgo; none
	// otherwise) followed by the three pure-Go engines, so this test passes
	// under both the cgo and CGO_ENABLED=0 build lanes.
	var want []string
	for _, e := range cgoEngines() {
		want = append(want, e.Name())
	}
	want = append(want, "go-flate", "klauspost-flate", "klauspost-zstd")
	if len(names) != len(want) {
		t.Fatalf("got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
}
