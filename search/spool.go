// Package search evaluates candidate engine parameters against a reference
// compressed stream.
package search

import (
	"bytes"
	"io"
	"os"
)

// Spool holds uncompressed content for the duration of a search. It starts
// in memory and spills to an unlinked temp file once it exceeds maxMem.
type Spool struct {
	dir    string
	maxMem int64
	buf    []byte
	file   *os.File
	size   int64
}

// NewSpool returns an empty spool that spills to dir above maxMem bytes.
func NewSpool(dir string, maxMem int64) *Spool {
	return &Spool{dir: dir, maxMem: maxMem}
}

func (s *Spool) Write(p []byte) (int, error) {
	if s.file == nil {
		if int64(len(s.buf))+int64(len(p)) <= s.maxMem {
			s.buf = append(s.buf, p...)
			s.size += int64(len(p))
			return len(p), nil
		}
		if err := s.spill(); err != nil {
			return 0, err
		}
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

func (s *Spool) spill() error {
	f, err := os.CreateTemp(s.dir, "comp-prysm-spool-*")
	if err != nil {
		return err
	}
	// Unlink immediately: the descriptor keeps the data alive and a crash
	// leaves nothing behind.
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(s.buf); err != nil {
		f.Close()
		return err
	}
	s.file = f
	s.buf = nil
	return nil
}

// Size is the number of bytes written so far.
func (s *Spool) Size() int64 { return s.size }

// InMemory reports whether the content is still held in memory.
func (s *Spool) InMemory() bool { return s.file == nil }

// Reader returns a new reader over the whole content. Readers are
// independent and may be used concurrently.
func (s *Spool) Reader() io.Reader {
	if s.file == nil {
		return bytes.NewReader(s.buf)
	}
	return io.NewSectionReader(s.file, 0, s.size)
}

// Close releases the memory or file backing the spool.
func (s *Spool) Close() error {
	s.buf = nil
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
