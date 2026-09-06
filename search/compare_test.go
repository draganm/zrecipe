package search

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCompareWriterMatches(t *testing.T) {
	ref := []byte("abcdefghij")
	cw := NewCompare(context.Background(), bytes.NewReader(ref))
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
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("abcdef")))
	cw.Write([]byte("abc"))
	_, err := cw.Write([]byte("dXf"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceShort(t *testing.T) {
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("ab")))
	_, err := cw.Write([]byte("abc"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceLong(t *testing.T) {
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("abc")))
	cw.Write([]byte("ab"))
	if err := cw.AtEOF(); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cw := NewCompare(ctx, bytes.NewReader([]byte("abc")))
	if _, err := cw.Write([]byte("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterLimit(t *testing.T) {
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("abcdefghij")))
	cw.SetLimit(5)
	if _, err := cw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if cw.Limited() {
		t.Fatal("limited after 3 of 5 bytes")
	}
	// The write that reaches the limit is compared whole and counted.
	n, err := cw.Write([]byte("defg"))
	if n != 4 || !errors.Is(err, ErrLimit) {
		t.Fatalf("got %d, %v", n, err)
	}
	if !cw.Limited() || cw.Err() != nil || cw.Matched() != 7 {
		t.Fatalf("limited %v, err %v, matched %d", cw.Limited(), cw.Err(), cw.Matched())
	}
	if _, err := cw.Write([]byte("hij")); !errors.Is(err, ErrLimit) {
		t.Fatalf("write after the limit: %v", err)
	}
	if cw.Matched() != 7 {
		t.Fatalf("matched %d after the limit, want 7", cw.Matched())
	}
}

func TestCompareWriterLimitMismatchBeforeLimit(t *testing.T) {
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("abcdefghij")))
	cw.SetLimit(5)
	cw.Write([]byte("abc"))
	if _, err := cw.Write([]byte("dXfg")); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
	if cw.Limited() || !errors.Is(cw.Err(), ErrMismatch) {
		t.Fatalf("limited %v, err %v", cw.Limited(), cw.Err())
	}
}

func TestCompareWriterNoLimitRunsToTheEnd(t *testing.T) {
	cw := NewCompare(context.Background(), bytes.NewReader([]byte("abc")))
	if _, err := cw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if cw.Limited() {
		t.Fatal("limited without a limit")
	}
	if err := cw.AtEOF(); err != nil {
		t.Fatal(err)
	}
}
