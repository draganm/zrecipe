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
	want := []string{"zlib", "libzstd", "go-flate", "klauspost-flate", "klauspost-zstd"}
	if len(names) != len(want) {
		t.Fatalf("got %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
}
