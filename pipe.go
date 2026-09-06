package zrecipe

import (
	"io"
	"sync"
)

// pipe is an in-memory pipe with a bounded queue of pending writes between
// its two ends. io.Pipe hands over one Write at a time, so a producer and a
// consumer joined by it run in lockstep, each idling while the other
// works; with a queue the producer runs ahead by up to slots writes and
// the two overlap. Confirm uses it between the goroutine reading the spool
// and the one rebuilding the input from it.
//
// Write and CloseWrite are called by the producer, Read and CloseRead by
// the consumer; each end is used from one goroutine at a time. Every write
// is copied, so the producer may reuse its buffer.
type pipe struct {
	ch     chan []byte
	closed chan struct{} // closed by CloseRead
	once   sync.Once
	cur    []byte // unread remainder of the last write taken from ch
	werr   error  // CloseWrite's error, read after ch is closed
}

// newPipe returns a pipe whose producer can be up to slots writes ahead of
// its consumer.
func newPipe(slots int) *pipe {
	return &pipe{ch: make(chan []byte, slots), closed: make(chan struct{})}
}

// Write queues a copy of b, blocking while the queue is full. It returns
// io.ErrClosedPipe once the consumer has called CloseRead.
func (p *pipe) Write(b []byte) (int, error) {
	select {
	case <-p.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case p.ch <- cp:
		return len(b), nil
	case <-p.closed:
		return 0, io.ErrClosedPipe
	}
}

// CloseWrite ends the stream: after the queued writes the consumer reads
// err, or io.EOF when err is nil. Write must not be called afterwards.
func (p *pipe) CloseWrite(err error) {
	p.werr = err
	close(p.ch)
}

// Read copies queued bytes into b, blocking while nothing is queued. It
// returns io.ErrClosedPipe after CloseRead.
func (p *pipe) Read(b []byte) (int, error) {
	for len(p.cur) == 0 {
		select {
		case <-p.closed:
			return 0, io.ErrClosedPipe
		default:
		}
		select {
		case c, ok := <-p.ch:
			if !ok {
				if p.werr != nil {
					return 0, p.werr
				}
				return 0, io.EOF
			}
			p.cur = c
		case <-p.closed:
			return 0, io.ErrClosedPipe
		}
	}
	n := copy(b, p.cur)
	p.cur = p.cur[n:]
	return n, nil
}

// CloseRead releases the producer: a Write that is blocked or comes later
// returns io.ErrClosedPipe, and so does any further Read. It is safe to
// call more than once.
func (p *pipe) CloseRead() {
	p.once.Do(func() { close(p.closed) })
}
