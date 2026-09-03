package search

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCompareWriterMatches(t *testing.T) {
	ref := []byte("abcdefghij")
	cw := newCompareWriter(context.Background(), bytes.NewReader(ref))
	for _, chunk := range [][]byte{[]byte("abc"), []byte("defg"), []byte("hij")} {
		if _, err := cw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := cw.AtEOF(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareWriterDiffers(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("abcdef")))
	cw.Write([]byte("abc"))
	_, err := cw.Write([]byte("dXf"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceShort(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("ab")))
	_, err := cw.Write([]byte("abc"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceLong(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("abc")))
	cw.Write([]byte("ab"))
	if err := cw.AtEOF(); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cw := newCompareWriter(ctx, bytes.NewReader([]byte("abc")))
	if _, err := cw.Write([]byte("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
