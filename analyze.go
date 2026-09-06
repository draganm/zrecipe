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
	// Uncompressed, if set, receives the decompressed content during the
	// first pass.
	Uncompressed io.Writer
	// Engines to search. Default DefaultEngines().
	Engines []engine.Engine
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
// streams, and searches for an engine and parameters that reproduce r
// exactly. For an uncompressed input it returns Params with FormatNone.
func Analyze(ctx context.Context, r io.ReadSeeker, opts *Options) (*Params, error) {
	o := opts.withDefaults()
	f, err := format.Detect(r)
	if err != nil {
		return nil, err
	}
	switch f {
	case FormatGzip:
		return analyzeGzip(ctx, r, o)
	case FormatZstd:
		return analyzeZstd(ctx, r, o)
	default:
		return analyzeNone(r, o)
	}
}

func analyzeNone(r io.Reader, o *Options) (*Params, error) {
	h := newHasher()
	w := io.Writer(h)
	if o.Uncompressed != nil {
		w = io.MultiWriter(h, o.Uncompressed)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return nil, err
	}
	d := digestOf(h, n)
	return &Params{Version: ParamsVersion, Format: FormatNone, Compressed: d, Uncompressed: d}, nil
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
// writes observe ctx: pass 1 has no other point where cancellation is
// checked, so without this a cancelled Analyze would still run to
// completion decompressing and hashing the whole input.
func spoolWriters(ctx context.Context, o *Options, sp *search.Spool, extra ...io.Writer) io.Writer {
	ws := append([]io.Writer{sp}, extra...)
	if o.Uncompressed != nil {
		ws = append(ws, o.Uncompressed)
	}
	return &ctxWriter{ctx: ctx, w: io.MultiWriter(ws...)}
}

func analyzeGzip(ctx context.Context, r io.ReadSeeker, o *Options) (*Params, error) {
	compHash := newHasher()
	cr := &countingReader{r: io.TeeReader(r, compHash)}
	br := bufio.NewReaderSize(cr, 64<<10)
	hdr, err := format.ParseGzipHeader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	sp := search.NewSpool(o.TempDir, o.MaxInMemory)
	defer sp.Close()
	uncHash := newHasher()
	crc := crc32.NewIEEE()
	fr := flate.NewReader(br)
	n, err := io.Copy(spoolWriters(ctx, o, sp, uncHash, crc), fr)
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
	in := &search.Input{Format: FormatGzip, Trailer: trailer[:], Spool: sp, UncompressedSize: n}
	in.Payload, in.Concurrent = payloadSource(r, int64(len(hdr.Raw)), cr.n)
	res, err := runSearch(ctx, in, gzipCandidates(o.Engines, hdr, n), o.Parallelism)
	if err != nil {
		return nil, err
	}
	return &Params{
		Version:       ParamsVersion,
		Format:        FormatGzip,
		Compressed:    digestOf(compHash, cr.n),
		Uncompressed:  digestOf(uncHash, n),
		Engine:        res.Candidate.Engine.Name(),
		EngineVersion: res.Candidate.Engine.Version(),
		Gzip:          &GzipParams{HeaderB64: base64.StdEncoding.EncodeToString(hdr.Raw), DeflateParams: *res.Candidate.Deflate},
	}, nil
}

func analyzeZstd(ctx context.Context, r io.ReadSeeker, o *Options) (*Params, error) {
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
	defer sp.Close()
	uncHash := newHasher()
	n, err := io.Copy(spoolWriters(ctx, o, sp, uncHash), dec)
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
	in := &search.Input{Format: FormatZstd, Spool: sp, UncompressedSize: n}
	in.Payload, in.Concurrent = payloadSource(r, 0, cr.n)
	res, err := runSearch(ctx, in, zstdCandidates(o.Engines, hdr, n), o.Parallelism)
	if err != nil {
		return nil, err
	}
	return &Params{
		Version:       ParamsVersion,
		Format:        FormatZstd,
		Compressed:    digestOf(compHash, cr.n),
		Uncompressed:  digestOf(uncHash, n),
		Engine:        res.Candidate.Engine.Name(),
		EngineVersion: res.Candidate.Engine.Version(),
		Zstd:          res.Candidate.Zstd,
	}, nil
}

func runSearch(ctx context.Context, in *search.Input, cands []search.Candidate, parallelism int) (*search.Result, error) {
	res, err := search.Run(ctx, in, cands, parallelism)
	if err != nil {
		if errors.Is(err, search.ErrNoMatch) {
			return nil, fmt.Errorf("%w: %v", ErrNotReproducible, err)
		}
		return nil, err
	}
	return res, nil
}

func gzipCandidates(engines []engine.Engine, h *format.GzipHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate // tiers[i] holds every engine's tier i, in engine order
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

func zstdCandidates(engines []engine.Engine, h *format.ZstdFrameHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate
	for _, e := range engines {
		ze, ok := e.(engine.ZstdEngine)
		if !ok {
			continue
		}
		for i, tier := range ze.Candidates(h, size) {
			for len(tiers) <= i {
				tiers = append(tiers, nil)
			}
			for _, p := range tier {
				p := p
				tiers[i] = append(tiers[i], search.Candidate{Engine: e, Zstd: &p})
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
