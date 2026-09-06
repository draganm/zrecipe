package zrecipe

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
	"github.com/draganm/zrecipe/search"
)

// confirmPipeSlots is how many FeedSize blocks the reader of the confirming
// pass may be ahead of the rebuilder.
const confirmPipeSlots = 8

// Analysis is a decompressed input together with the candidate the
// elimination settled on, waiting for Confirm to reproduce the input from
// the content while streaming that content to the caller. Close releases
// the spool holding the content.
type Analysis struct {
	o       *Options
	r       io.ReadSeeker
	format  Format
	spool   *search.Spool                      // nil for FormatNone
	payload func(off int64) (io.Reader, error) // the whole input from offset off
	params  *Params

	verified  bool
	confirmed bool
	closed    bool
}

// Start detects the format of r, decompresses it once into a spool while
// hashing both streams, validates the container, and eliminates the
// candidates down to one. It returns ErrNotReproducible when every
// candidate diverged from the input inside the elimination, and
// ErrUnsupported, ErrCorrupt or the context's error as Analyze does. For an
// uncompressed input it hashes r and there is nothing to eliminate. The
// caller must Close the returned Analysis.
func Start(ctx context.Context, r io.ReadSeeker, opts *Options) (*Analysis, error) {
	o := opts.withDefaults()
	f, err := format.Detect(r)
	if err != nil {
		return nil, err
	}
	a := &Analysis{o: o, r: r, format: f}
	var p1 *passOne
	switch f {
	case FormatGzip:
		p1, err = passOneGzip(ctx, r, o)
	case FormatZstd:
		p1, err = passOneZstd(ctx, r, o)
	default:
		return a, a.startNone(ctx)
	}
	if err != nil {
		return nil, err
	}
	res, err := eliminate(ctx, p1.in, p1.cands, o.Parallelism)
	if err != nil {
		p1.spool.Close()
		return nil, err
	}
	a.spool = p1.spool
	a.params = p1.withCandidate(res.Candidate)
	a.verified = res.Verified
	a.payload, _ = payloadSource(r, 0, p1.size)
	return a, nil
}

// startNone hashes an uncompressed input once; Confirm copies it again.
func (a *Analysis) startNone(ctx context.Context) error {
	if _, err := a.r.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := newHasher()
	n, err := io.Copy(&ctxWriter{ctx: ctx, w: h}, a.r)
	if err != nil {
		return err
	}
	d := digestOf(h, n)
	a.params = &Params{Version: ParamsVersion, Format: FormatNone, Compressed: d, Uncompressed: d}
	a.verified = true
	return nil
}

// Format is the container format of the input.
func (a *Analysis) Format() Format { return a.format }

// Compressed is the BLAKE3 digest and size of the input.
func (a *Analysis) Compressed() Digest { return a.params.Compressed }

// Uncompressed is the BLAKE3 digest and size of the decompressed content.
func (a *Analysis) Uncompressed() Digest { return a.params.Uncompressed }

// Verified reports that the elimination already reproduced the whole input
// (a small input, one the fallback search ran to the end, or an
// uncompressed one). Confirm runs the rebuild regardless, so callers need
// not care; it is exposed for logs and tests.
func (a *Analysis) Verified() bool { return a.verified }

// Confirm reads the content once, writes every block to tee (nil to skip)
// and rebuilds the input from it through Recompress's own code, comparing
// the output with the input byte for byte. It returns the Params on
// success; ErrNotReproducible, naming the offset, when the output diverges;
// tee's own error, wrapped, when a tee write fails; the context's error; or
// an engine error. After a failure tee has received a prefix of the
// content. A block reaches tee before the rebuilder sees it, so tee may be
// a few blocks ahead of the comparison when the pass stops. With
// Options.VerifyLimit set the rebuild stops once that many bytes matched
// and the rest of the content goes to tee alone. Confirm may be called
// once.
func (a *Analysis) Confirm(ctx context.Context, tee io.Writer) (*Params, error) {
	if a.closed {
		return nil, errors.New("zrecipe: Confirm after Close")
	}
	if a.confirmed {
		return nil, errors.New("zrecipe: Confirm called twice")
	}
	a.confirmed = true
	if tee == nil {
		tee = io.Discard
	}
	if a.format == FormatNone {
		return a.confirmNone(ctx, tee)
	}

	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	p := newPipe(confirmPipeSlots)
	var teeErr, readErr error
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, engine.FeedSize)
		src := a.spool.Reader()
		feeding := true // the rebuilder is still reading the pipe
		for {
			if err := cctx.Err(); err != nil {
				p.CloseWrite(context.Cause(cctx))
				return
			}
			n, err := io.ReadFull(src, buf)
			if n > 0 {
				if _, werr := tee.Write(buf[:n]); werr != nil {
					teeErr = werr
					cancel(werr)
					p.CloseWrite(werr)
					return
				}
				if feeding {
					if _, werr := p.Write(buf[:n]); werr != nil {
						// The rebuilder has stopped reading: satisfied at
						// the verify limit, so the rest of the content
						// goes to tee alone, or failed, in which case it
						// cancelled cctx and the next round sees that.
						feeding = false
					}
				}
			}
			switch {
			case err == nil:
			case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
				p.CloseWrite(nil)
				return
			default:
				readErr = err
				cancel(err)
				p.CloseWrite(err)
				return
			}
		}
	}()

	var rerr error
	var limited bool // the rebuild matched up to the verify limit and stopped
	ref, err := a.payload(0)
	if err != nil {
		rerr = err
	} else {
		cw := search.NewCompare(cctx, ref)
		cw.SetLimit(a.o.VerifyLimit)
		rerr = rebuild(cctx, a.params, p, cw, &RecompressOptions{Engines: a.o.Engines})
		limited = cw.Limited()
		if rerr == nil && !limited {
			rerr = cw.AtEOF()
		}
	}
	if rerr != nil && !limited {
		cancel(rerr)
	}
	// Release the reader from the pipe. Satisfied at the limit, the
	// rebuilder leaves the reader to stream the rest of the content to
	// tee; otherwise the reader is done or about to see the cancellation.
	p.CloseRead()
	<-readDone

	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case teeErr != nil:
		return nil, fmt.Errorf("zrecipe: uncompressed writer: %w", teeErr)
	case readErr != nil:
		return nil, fmt.Errorf("zrecipe: spool: %w", readErr)
	case limited:
		return a.params, nil
	case rerr != nil && errors.Is(rerr, search.ErrMismatch):
		return nil, fmt.Errorf("%w: %w", ErrNotReproducible, rerr)
	case rerr != nil:
		return nil, rerr
	}
	return a.params, nil
}

// confirmNone copies an uncompressed input to tee; there is nothing to
// rebuild.
func (a *Analysis) confirmNone(ctx context.Context, tee io.Writer) (*Params, error) {
	if _, err := a.r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	tw := &teeWriter{w: tee}
	if _, err := io.Copy(&ctxWriter{ctx: ctx, w: tw}, a.r); err != nil {
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case tw.err != nil:
			return nil, fmt.Errorf("zrecipe: uncompressed writer: %w", tw.err)
		}
		return nil, err
	}
	return a.params, nil
}

// teeWriter remembers the first error a writer returned.
type teeWriter struct {
	w   io.Writer
	err error
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if err != nil && t.err == nil {
		t.err = err
	}
	return n, err
}

// Close releases the spool. It is idempotent; Confirm fails after it.
func (a *Analysis) Close() error {
	if a.closed {
		return nil
	}
	a.closed = true
	if a.spool != nil {
		return a.spool.Close()
	}
	return nil
}
