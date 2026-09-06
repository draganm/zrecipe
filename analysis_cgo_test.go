//go:build cgo

package zrecipe

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/engine/goflate"
	"github.com/draganm/zrecipe/enginetest"
	"github.com/draganm/zrecipe/fixtures"
)

// TestStartConfirmRunsPigzCandidatesRaceFree drives Start with the default
// engines, whose pigz candidates compress on worker goroutines and write
// the comparison writer asynchronously while the elimination reads its
// state between windows. The input is large enough that pigz's default and
// smaller blocks fill and emit during the elimination rather than only at
// Close, so the concurrent access is real. Run under -race it guards the
// fix that made Compare synchronized and gave each candidate its own
// reference reader. The winner is a go-flate level, but every candidate,
// pigz included, is exercised on the way there.
func TestStartConfirmRunsPigzCandidatesRaceFree(t *testing.T) {
	data := fixtures.Mixed(3 << 20)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	for i := 0; i < 3; i++ {
		a, err := Start(context.Background(), bytes.NewReader(file), &Options{Parallelism: 8})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		var tee bytes.Buffer
		p, err := a.Confirm(context.Background(), &tee)
		a.Close()
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if p.Engine != "go-flate" || !bytes.Equal(tee.Bytes(), data) {
			t.Fatalf("engine %s, tee %d bytes", p.Engine, tee.Len())
		}
	}
}

// TestStartConfirmPigzProducerRaceFree is the same guard for a file pigz
// actually produced, so a pigz candidate is the winner and runs to the end
// of the elimination writing its output asynchronously throughout.
func TestStartConfirmPigzProducerRaceFree(t *testing.T) {
	tool(t, "pigz")
	data := fixtures.Mixed(3 << 20)
	file := run(t, data, "", "pigz", "-c", "-6")
	a, err := Start(context.Background(), bytes.NewReader(file), &Options{Parallelism: 8})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer a.Close()
	p, err := a.Confirm(context.Background(), io.Discard)
	if err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if p.Engine != "pigz" {
		t.Fatalf("engine = %s, want pigz", p.Engine)
	}
}
