package goflate

import (
	"runtime"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "go-flate" || e.Version() != runtime.Version() || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s %s", e.Name(), e.Version(), e.Format())
	}
}

func TestCandidatesFollowXFL(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 0)
	if len(tiers) != 1 || len(tiers[0]) != 11 || tiers[0][0].Level != 9 || tiers[0][10].Level != -2 {
		t.Fatalf("%+v", tiers)
	}
}

func TestRejectsZlibOnlyParams(t *testing.T) {
	if _, err := New().NewWriter(nil, engine.DeflateParams{Level: 6, MemLevel: 8}); err == nil {
		t.Fatal("expected error for mem_level")
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}
