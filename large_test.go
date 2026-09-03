package compprysm

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/draganm/comp-prysm/fixtures"
)

// TestLargeInput streams a 2 GiB gzip through Analyze and Recompress. It
// runs only with COMP_PRYSM_LARGE=1 because it takes minutes and disk.
func TestLargeInput(t *testing.T) {
	if os.Getenv("COMP_PRYSM_LARGE") == "" {
		t.Skip("set COMP_PRYSM_LARGE=1 to run")
	}
	const size = 2 << 30
	dir := t.TempDir()
	unc := filepath.Join(dir, "big")
	gz := filepath.Join(dir, "big.gz")

	uf, _ := os.Create(unc)
	gf, _ := os.Create(gz)
	zw, _ := gzip.NewWriterLevel(gf, gzip.BestSpeed)
	chunk := fixtures.Mixed(4 << 20)
	w := io.MultiWriter(uf, zw)
	for written := 0; written < size; written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	gf.Close()
	uf.Close()

	in, _ := os.Open(gz)
	defer in.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, err := Analyze(context.Background(), in, &Options{TempDir: dir, Parallelism: 2})
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	t.Logf("engine %s %+v; heap grew by %d MiB", p.Engine, p.Gzip.DeflateParams, (after.HeapAlloc-before.HeapAlloc)>>20)
	if p.Uncompressed.Size != size {
		t.Fatalf("size %d", p.Uncompressed.Size)
	}
	const heapBound = 256 << 20
	if after.HeapInuse >= heapBound {
		t.Fatalf("HeapInuse = %d MiB, want < %d MiB (spooling and searching a 2 GiB input must not hold it in memory)", after.HeapInuse>>20, heapBound>>20)
	}
	t.Logf("HeapInuse after Analyze: %d MiB", after.HeapInuse>>20)

	src, _ := os.Open(unc)
	defer src.Close()
	if err := Recompress(context.Background(), p, src, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
}
