package search

import (
	"bytes"
	"io"
	"testing"
)

func TestSpoolStaysInMemoryBelowLimit(t *testing.T) {
	s := NewSpool(t.TempDir(), 100)
	defer s.Close()
	s.Write([]byte("hello "))
	s.Write([]byte("world"))
	if !s.InMemory() || s.Size() != 11 {
		t.Fatalf("in memory %v size %d", s.InMemory(), s.Size())
	}
	got, _ := io.ReadAll(s.Reader())
	if string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestSpoolSpillsToFile(t *testing.T) {
	s := NewSpool(t.TempDir(), 8)
	defer s.Close()
	s.Write([]byte("12345"))
	if !s.InMemory() {
		t.Fatal("spilled too early")
	}
	s.Write([]byte("6789"))
	if s.InMemory() {
		t.Fatal("did not spill")
	}
	s.Write([]byte("abc"))
	if s.Size() != 12 {
		t.Fatalf("size %d", s.Size())
	}
	a, _ := io.ReadAll(s.Reader())
	b, _ := io.ReadAll(s.Reader())
	if string(a) != "123456789abc" || !bytes.Equal(a, b) {
		t.Fatalf("got %q and %q", a, b)
	}
}

func TestSpoolReadersAreIndependent(t *testing.T) {
	s := NewSpool(t.TempDir(), 4)
	defer s.Close()
	s.Write([]byte("abcdefgh"))
	r1, r2 := s.Reader(), s.Reader()
	b1 := make([]byte, 3)
	io.ReadFull(r1, b1)
	b2 := make([]byte, 3)
	io.ReadFull(r2, b2)
	if string(b1) != "abc" || string(b2) != "abc" {
		t.Fatalf("%q %q", b1, b2)
	}
}

func TestSpoolEmpty(t *testing.T) {
	s := NewSpool(t.TempDir(), 4)
	defer s.Close()
	got, _ := io.ReadAll(s.Reader())
	if len(got) != 0 || s.Size() != 0 {
		t.Fatal("expected empty")
	}
}

func TestSpoolSection(t *testing.T) {
	for _, maxMem := range []int64{1 << 20, 10} { // in memory, spilled
		sp := NewSpool(t.TempDir(), maxMem)
		if _, err := sp.Write([]byte("0123456789abcdef")); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(sp.Section(4, 6))
		if err != nil || string(got) != "456789" {
			t.Fatalf("maxMem %d: %q, %v", maxMem, got, err)
		}
		got, err = io.ReadAll(sp.Section(14, 2))
		if err != nil || string(got) != "ef" {
			t.Fatalf("maxMem %d: %q, %v", maxMem, got, err)
		}
		if sp.InMemory() != (maxMem > 16) {
			t.Fatalf("maxMem %d: InMemory = %v", maxMem, sp.InMemory())
		}
		sp.Close()
	}
}
