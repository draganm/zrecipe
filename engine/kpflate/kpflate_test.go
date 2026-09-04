package kpflate

import (
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "klauspost-flate" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	// Note: go test binaries omit the dependency list from runtime/debug.ReadBuildInfo,
	// so engine.ModuleVersion returns "(devel)". The actual version v1.20.0 is observable
	// only in a built binary (e.g., the CLI), which the integration tests cover.
	v := e.Version()
	if v != "v1.20.0" && v != "(devel)" {
		t.Fatalf("version %q; update this test when bumping klauspost/compress", v)
	}
}

func TestCandidates(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 4}, 0)
	if len(tiers) != 1 || len(tiers[0]) != 11 || tiers[0][0].Level != 1 || tiers[0][10].Level != -2 {
		t.Fatalf("%+v", tiers)
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}
