package zrecipe

import (
	"bufio"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/zrecipe/engine"
	"github.com/draganm/zrecipe/format"
	"github.com/draganm/zrecipe/search"
)

// DefaultMaxInMemory is the spool size above which content goes to a temp file.
const DefaultMaxInMemory = 64 << 20

// Options configures Analyze. The zero value uses the defaults.
type Options struct {
	// TempDir holds the spool for large inputs. Default os.TempDir().
	TempDir string
	// MaxInMemory is the spool size kept in memory. Default DefaultMaxInMemory.
	MaxInMemory int64
	// Parallelism is the number of candidates evaluated at once. Default
	// runtime.NumCPU(). Takes effect only when r implements io.ReaderAt.
	Parallelism int
	// Uncompressed, if set, receives the decompressed content while
	// Analyze confirms the parameters it found (the confirming pass, see
	// Analysis.Confirm), in order and in engine.FeedSize writes. An input
	// that is not reproducible writes nothing to it; one whose confirmation
	// fails writes a prefix.
	Uncompressed io.Writer
	// Engines to search. Default DefaultEngines().
	Engines []engine.Engine
	// VerifyLimit, when positive, accepts a candidate once it has
	// reproduced this many bytes of the compressed input instead of
	// running it to the end, both in the search and in the confirming
	// pass: a candidate that matches that far and diverges later is rare
	// enough that recompressing the rest of a large input is not worth
	// its time. The confirming pass still streams the whole content to
	// its tee. Zero, the default, verifies the whole input. Recompress
	// checks the output digest, so a divergence past the limit surfaces
	// there.
	VerifyLimit int64
}

func (o *Options) withDefaults() *Options {
	out := Options{}
	if o != nil {
		out = *o
	}
	if out.TempDir == "" {
		out.TempDir = os.TempDir()
	}
	if out.MaxInMemory <= 0 {
		out.MaxInMemory = DefaultMaxInMemory
	}
	if out.Parallelism <= 0 {
		out.Parallelism = runtime.NumCPU()
	}
	if out.Engines == nil {
		out.Engines = DefaultEngines()
	}
	return &out
}

// Analyze detects the format of r, decompresses it once while hashing both
// streams, finds an engine and parameters that reproduce r exactly and
// confirms them through the pull path: it is Start, Confirm with
// Options.Uncompressed as the tee, and Close. For an uncompressed input it
// returns Params with FormatNone.
func Analyze(ctx context.Context, r io.ReadSeeker, opts *Options) (*Params, error) {
	o := opts.withDefaults()
	a, err := Start(ctx, r, o)
	if err != nil {
		return nil, err
	}
	defer a.Close()
	return a.Confirm(ctx, o.Uncompressed)
}

// passOne is what the first pass over a compressed input leaves for the
// elimination: the spool, the search input and its candidates, the number
// of compressed bytes, and the Params with everything but the engine
// filled in.
type passOne struct {
	spool  *search.Spool
	in     *search.Input
	cands  []search.Candidate
	size   int64
	params *Params
}

// withCandidate completes the Params for c.
func (p *passOne) withCandidate(c search.Candidate) *Params {
	out := *p.params
	out.Engine = c.Engine.Name()
	out.EngineVersion = c.Engine.Version()
	if c.Deflate != nil {
		g := *p.params.Gzip
		g.DeflateParams = *c.Deflate
		out.Gzip = &g
	}
	if c.Zstd != nil {
		out.Zstd = c.Zstd
	}
	return &out
}

// payloadSource returns a factory for readers over r from base+off to size,
// and whether those readers may be used concurrently.
func payloadSource(r io.ReadSeeker, base, size int64) (func(off int64) (io.Reader, error), bool) {
	if ra, ok := r.(io.ReaderAt); ok {
		return func(off int64) (io.Reader, error) {
			return io.NewSectionReader(ra, base+off, size-base-off), nil
		}, true
	}
	return func(off int64) (io.Reader, error) {
		if _, err := r.Seek(base+off, io.SeekStart); err != nil {
			return nil, err
		}
		return r, nil
	}, false
}

// spoolWriters builds the fan-out for the decompressed stream, wrapped so
// writes observe ctx: pass one has no other point where cancellation is
// checked, so without this a cancelled Start would still run to
// completion decompressing and hashing the whole input.
func spoolWriters(ctx context.Context, sp *search.Spool, extra ...io.Writer) io.Writer {
	ws := append([]io.Writer{sp}, extra...)
	return &ctxWriter{ctx: ctx, w: io.MultiWriter(ws...)}
}

// passOneGzip inflates r once into a spool, hashing both streams and
// checking the member's trailer, and lists the deflate candidates. The
// spool is the caller's to close on success.
func passOneGzip(ctx context.Context, r io.ReadSeeker, o *Options) (_ *passOne, err error) {
	compHash := newHasher()
	cr := &countingReader{r: io.TeeReader(r, compHash)}
	br := bufio.NewReaderSize(cr, 64<<10)
	hdr, err := format.ParseGzipHeader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	sp := search.NewSpool(o.TempDir, o.MaxInMemory)
	defer func() {
		if err != nil {
			sp.Close()
		}
	}()
	uncHash := newHasher()
	crc := crc32.NewIEEE()
	fr := flate.NewReader(br)
	n, err := io.Copy(spoolWriters(ctx, sp, uncHash, crc), fr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: deflate: %v", ErrCorrupt, err)
	}
	fr.Close()
	var trailer [8]byte
	if _, err := io.ReadFull(br, trailer[:]); err != nil {
		return nil, fmt.Errorf("%w: gzip trailer: %v", ErrCorrupt, err)
	}
	if binary.LittleEndian.Uint32(trailer[0:4]) != crc.Sum32() || binary.LittleEndian.Uint32(trailer[4:8]) != uint32(n) {
		return nil, fmt.Errorf("%w: gzip trailer does not match content", ErrCorrupt)
	}
	extra, err := io.Copy(io.Discard, br)
	if err != nil {
		return nil, err
	}
	if extra > 0 {
		return nil, fmt.Errorf("%w: multi-member gzip (%d bytes after the first member)", ErrUnsupported, extra)
	}
	in := &search.Input{Format: FormatGzip, Trailer: trailer[:], Spool: sp, UncompressedSize: n, VerifyLimit: o.VerifyLimit}
	in.Payload, in.Concurrent = payloadSource(r, int64(len(hdr.Raw)), cr.n)
	return &passOne{
		spool: sp,
		in:    in,
		cands: gzipCandidates(o.Engines, hdr, n),
		size:  cr.n,
		params: &Params{
			Version:      ParamsVersion,
			Format:       FormatGzip,
			Compressed:   digestOf(compHash, cr.n),
			Uncompressed: digestOf(uncHash, n),
			Gzip:         &GzipParams{HeaderB64: base64.StdEncoding.EncodeToString(hdr.Raw)},
		},
	}, nil
}

// passOneZstd is passOneGzip for a zstd frame.
func passOneZstd(ctx context.Context, r io.ReadSeeker, o *Options) (_ *passOne, err error) {
	// Pass 0: find the frame extent without decoding so the decoder can be
	// bounded to exactly one frame.
	hdr, frameLen, err := format.ZstdFrameLength(bufio.NewReaderSize(r, 64<<10))
	if err != nil {
		if errors.Is(err, format.ErrSkippableFrame) {
			return nil, fmt.Errorf("%w: zstd skippable frame", ErrUnsupported)
		}
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if hdr.DictID != 0 {
		return nil, fmt.Errorf("%w: zstd dictionary %d", ErrUnsupported, hdr.DictID)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	compHash := newHasher()
	cr := &countingReader{r: io.TeeReader(r, compHash)}
	br := bufio.NewReaderSize(cr, 64<<10)
	dec, err := zstd.NewReader(io.LimitReader(br, frameLen), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	sp := search.NewSpool(o.TempDir, o.MaxInMemory)
	defer func() {
		if err != nil {
			sp.Close()
		}
	}()
	uncHash := newHasher()
	n, err := io.Copy(spoolWriters(ctx, sp, uncHash), dec)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
	}
	if hdr.HasContentSize && hdr.ContentSize != uint64(n) {
		return nil, fmt.Errorf("%w: zstd content size %d, decoded %d", ErrCorrupt, hdr.ContentSize, n)
	}
	extra, err := io.Copy(io.Discard, br)
	if err != nil {
		return nil, err
	}
	if extra > 0 {
		return nil, fmt.Errorf("%w: multi-frame zstd (%d bytes after the first frame)", ErrUnsupported, extra)
	}
	in := &search.Input{Format: FormatZstd, Spool: sp, UncompressedSize: n, VerifyLimit: o.VerifyLimit}
	in.Payload, in.Concurrent = payloadSource(r, 0, cr.n)
	return &passOne{
		spool: sp,
		in:    in,
		cands: zstdCandidates(o.Engines, hdr, n),
		size:  cr.n,
		params: &Params{
			Version:      ParamsVersion,
			Format:       FormatZstd,
			Compressed:   digestOf(compHash, cr.n),
			Uncompressed: digestOf(uncHash, n),
		},
	}, nil
}

// eliminate runs the elimination and reports no survivor as
// ErrNotReproducible.
func eliminate(ctx context.Context, in *search.Input, cands []search.Candidate, parallelism int) (*search.Result, error) {
	res, err := search.Eliminate(ctx, in, cands, parallelism)
	if err != nil {
		if errors.Is(err, search.ErrNoMatch) {
			return nil, fmt.Errorf("%w: %v", ErrNotReproducible, err)
		}
		return nil, err
	}
	return res, nil
}

// gzipCandidates lists every engine's candidates, tier by tier: every
// engine's tier i, in engine order, before any engine's tier i+1. The
// tiers only order the list; the elimination runs over all of it, since a
// likely candidate may agree with an unlikely one over a long prefix and
// only the lockstep can tell them apart.
func gzipCandidates(engines []engine.Engine, h *format.GzipHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate
	for _, e := range engines {
		de, ok := e.(engine.DeflateEngine)
		if !ok {
			continue
		}
		for i, tier := range de.Candidates(h, size) {
			for len(tiers) <= i {
				tiers = append(tiers, nil)
			}
			for _, p := range tier {
				p := p
				tiers[i] = append(tiers[i], search.Candidate{Engine: e, Deflate: &p})
			}
		}
	}
	return flatten(tiers)
}

// zstdCandidates is gzipCandidates for zstd engines, carrying what each
// engine says its candidates buffer before their first output.
func zstdCandidates(engines []engine.Engine, h *format.ZstdFrameHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate
	for _, e := range engines {
		ze, ok := e.(engine.ZstdEngine)
		if !ok {
			continue
		}
		buffering, _ := e.(engine.ZstdBuffering)
		for i, tier := range ze.Candidates(h, size) {
			for len(tiers) <= i {
				tiers = append(tiers, nil)
			}
			for _, p := range tier {
				p := p
				c := search.Candidate{Engine: e, Zstd: &p}
				if buffering != nil {
					c.Buffered = buffering.Buffered(p, size)
				}
				tiers[i] = append(tiers[i], c)
			}
		}
	}
	return flatten(tiers)
}

func flatten(tiers [][]search.Candidate) []search.Candidate {
	var out []search.Candidate
	for _, t := range tiers {
		out = append(out, t...)
	}
	return out
}
