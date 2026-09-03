package kpflate

import (
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "klauspost-flate" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "v1.20.0" {
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
