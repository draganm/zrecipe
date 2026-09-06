package zrecipe

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestPipeDeliversWritesInOrderThenEOF(t *testing.T) {
	p := newPipe(2)
	go func() {
		for _, s := range []string{"ab", "cde", "f"} {
			if _, err := p.Write([]byte(s)); err != nil {
				t.Error(err)
			}
		}
		p.CloseWrite(nil)
	}()
	got, err := io.ReadAll(p)
	if err != nil || string(got) != "abcdef" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestPipeCopiesWrittenBytes(t *testing.T) {
	p := newPipe(4)
	buf := []byte("one")
	p.Write(buf)
	copy(buf, "two")
	p.CloseWrite(nil)
	got, _ := io.ReadAll(p)
	if string(got) != "one" {
		t.Fatalf("%q", got)
	}
}

func TestPipeCloseWriteErrorFollowsTheData(t *testing.T) {
	boom := errors.New("boom")
	p := newPipe(1)
	p.Write([]byte("data"))
	p.CloseWrite(boom)
	got, err := io.ReadAll(p)
	if string(got) != "data" || !errors.Is(err, boom) {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestPipeCloseReadUnblocksAndFailsTheWriter(t *testing.T) {
	p := newPipe(1)
	p.Write([]byte("fills the queue"))
	done := make(chan error, 1)
	go func() {
		_, err := p.Write([]byte("blocks"))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	p.CloseRead()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer stayed blocked")
	}
	if _, err := p.Write([]byte("later")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v", err)
	}
	if _, err := p.Read(make([]byte, 4)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v", err)
	}
}

func TestPipeCloseReadIsIdempotent(t *testing.T) {
	p := newPipe(1)
	p.CloseRead()
	p.CloseRead()
}
