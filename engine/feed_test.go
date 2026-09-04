package engine

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// recordingWriter keeps the size of every Write.
type recordingWriter struct {
	sizes []int
	buf   bytes.Buffer
}

func (r *recordingWriter) Write(p []byte) (int, error) {
	r.sizes = append(r.sizes, len(p))
	return r.buf.Write(p)
}

// oneKiB hands out at most 1 KiB per Read and, having no WriteTo, offers no
// fast path.
type oneKiB struct{ r io.Reader }

func (o oneKiB) Read(p []byte) (int, error) {
	if len(p) > 1024 {
		p = p[:1024]
	}
	return o.r.Read(p)
}

func TestFeedWritesFixedSizeChunksWhateverTheReader(t *testing.T) {
	data := make([]byte, 2*FeedSize+100)
	for i := range data {
		data[i] = byte(i)
	}
	want := []int{FeedSize, FeedSize, 100}
	// bytes.Reader implements io.WriterTo; oneKiB returns short reads.
	for name, src := range map[string]io.Reader{"WriterTo": bytes.NewReader(data), "1KiB-reads": oneKiB{bytes.NewReader(data)}} {
		w := &recordingWriter{}
		n, err := Feed(w, src)
		if err != nil || n != int64(len(data)) {
			t.Fatalf("%s: n=%d err=%v", name, n, err)
		}
		if !bytes.Equal(w.buf.Bytes(), data) {
			t.Fatalf("%s: content differs", name)
		}
		if len(w.sizes) != len(want) {
			t.Fatalf("%s: write sizes %v, want %v", name, w.sizes, want)
		}
		for i := range want {
			if w.sizes[i] != want[i] {
				t.Fatalf("%s: write sizes %v, want %v", name, w.sizes, want)
			}
		}
	}
}

func TestFeedExactMultipleAndEmpty(t *testing.T) {
	w := &recordingWriter{}
	if n, err := Feed(w, bytes.NewReader(make([]byte, 3*FeedSize))); err != nil || n != 3*FeedSize || len(w.sizes) != 3 {
		t.Fatalf("n=%d err=%v sizes=%v", n, err, w.sizes)
	}
	w = &recordingWriter{}
	if n, err := Feed(w, bytes.NewReader(nil)); err != nil || n != 0 || len(w.sizes) != 0 {
		t.Fatalf("empty: n=%d err=%v sizes=%v", n, err, w.sizes)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, io.ErrClosedPipe }

func TestFeedPropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	if _, err := Feed(&recordingWriter{}, io.MultiReader(bytes.NewReader(make([]byte, 10)), errReader{boom})); !errors.Is(err, boom) {
		t.Fatalf("read error: %v", err)
	}
	if _, err := Feed(errWriter{}, bytes.NewReader(make([]byte, 10))); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error: %v", err)
	}
}
