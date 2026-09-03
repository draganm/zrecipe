// Package enginetest holds conformance tests shared by all engines.
package enginetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
	"github.com/draganm/comp-prysm/search"
)

// Flatten concatenates tiers in order.
func Flatten[T any](tiers [][]T) []T {
	var out []T
	for _, t := range tiers {
		out = append(out, t...)
	}
	return out
}

// Gzip builds a complete gzip file from e with parameters p.
func Gzip(t testing.TB, e engine.DeflateEngine, p engine.DeflateParams, data []byte) []byte {
	t.Helper()
	var xfl byte
	switch p.Level {
	case 9:
		xfl = 2
	case 1:
		xfl = 4
	}
	var buf bytes.Buffer
	buf.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, xfl, 255})
	w, err := e.NewWriter(&buf, p)
	if err != nil {
		t.Fatalf("%s %+v: %v", e.Name(), p, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc32.ChecksumIEEE(data))
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(len(data)))
	buf.Write(trailer[:])
	return buf.Bytes()
}

// Zstd builds a complete zstd frame from e with parameters p.
func Zstd(t testing.TB, e engine.ZstdEngine, p engine.ZstdParams, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := e.NewWriter(&buf, p, int64(len(data)))
	if err != nil {
		t.Fatalf("%s %+v: %v", e.Name(), p, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func spool(t testing.TB, data []byte) *search.Spool {
	t.Helper()
	sp := search.NewSpool(t.TempDir(), 1<<20)
	if _, err := sp.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	return sp
}

// RoundTripDeflate compresses every fixture with every parameter set, then
// checks the search over all of e's candidates finds a reproducing one.
func RoundTripDeflate(t *testing.T, e engine.DeflateEngine, params []engine.DeflateParams, fx []fixtures.Fixture) {
	for _, f := range fx {
		for _, p := range params {
			t.Run(fmt.Sprintf("%s/%+v", f.Name, p), func(t *testing.T) {
				file := Gzip(t, e, p, f.Data)
				hdr, err := format.ParseGzipHeader(bufio.NewReader(bytes.NewReader(file)))
				if err != nil {
					t.Fatal(err)
				}
				var cands []search.Candidate
				for _, c := range Flatten(e.Candidates(hdr, int64(len(f.Data)))) {
					c := c
					cands = append(cands, search.Candidate{Engine: e, Deflate: &c})
				}
				in := &search.Input{
					Format:           format.Gzip,
					Payload:          func() (io.Reader, error) { return bytes.NewReader(file[len(hdr.Raw):]), nil },
					Concurrent:       true,
					Trailer:          file[len(file)-8:],
					Spool:            spool(t, f.Data),
					UncompressedSize: int64(len(f.Data)),
				}
				res, err := search.Run(context.Background(), in, cands, 4)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				again := Gzip(t, e, *res.Candidate.Deflate, f.Data)
				if !bytes.Equal(again, file) {
					t.Fatalf("found %+v but it does not reproduce the file", *res.Candidate.Deflate)
				}
			})
		}
	}
}

// RoundTripZstd is the zstd counterpart of RoundTripDeflate.
func RoundTripZstd(t *testing.T, e engine.ZstdEngine, params []engine.ZstdParams, fx []fixtures.Fixture) {
	for _, f := range fx {
		for _, p := range params {
			t.Run(fmt.Sprintf("%s/%+v", f.Name, p), func(t *testing.T) {
				file := Zstd(t, e, p, f.Data)
				hdr, err := format.ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(file)))
				if err != nil {
					t.Fatal(err)
				}
				var cands []search.Candidate
				for _, c := range Flatten(e.Candidates(hdr, int64(len(f.Data)))) {
					c := c
					cands = append(cands, search.Candidate{Engine: e, Zstd: &c})
				}
				in := &search.Input{
					Format:           format.Zstd,
					Payload:          func() (io.Reader, error) { return bytes.NewReader(file), nil },
					Concurrent:       true,
					Spool:            spool(t, f.Data),
					UncompressedSize: int64(len(f.Data)),
				}
				res, err := search.Run(context.Background(), in, cands, 4)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				again := Zstd(t, e, *res.Candidate.Zstd, f.Data)
				if !bytes.Equal(again, file) {
					t.Fatalf("found %+v but it does not reproduce the file", *res.Candidate.Zstd)
				}
			})
		}
	}
}
