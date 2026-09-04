package goflate

import (
	"runtime"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
	"github.com/draganm/zrecipe/format"
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

func TestRejectsOtherEnginesParams(t *testing.T) {
	if _, err := New().NewWriter(nil, engine.DeflateParams{Level: 6, MemLevel: 8}); err == nil {
		t.Fatal("expected error for mem_level")
	}
	if _, err := New().NewWriter(nil, engine.DeflateParams{Level: 6, Rsyncable: true}); err == nil {
		t.Fatal("expected error for rsyncable")
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}
