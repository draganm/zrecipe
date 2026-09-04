package zrecipe

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/draganm/zrecipe/engine"
)

// RecompressOptions configures Recompress. The zero value uses the defaults.
type RecompressOptions struct {
	// Engines to look the recorded engine up in. Default DefaultEngines().
	Engines []engine.Engine
	// AllowVersionMismatch tries the engine even when its version differs
	// from the recorded one. The digest check still decides the outcome.
	AllowVersionMismatch bool
}

// Recompress rebuilds the compressed file described by p from uncompressed
// and streams it to w. It verifies the input against p.Uncompressed and the
// output against p.Compressed. Bytes already written to w are not rolled
// back on error; write to a temporary file and rename on success.
func Recompress(ctx context.Context, p *Params, uncompressed io.Reader, w io.Writer, opts *RecompressOptions) error {
	o := RecompressOptions{}
	if opts != nil {
		o = *opts
	}
	if o.Engines == nil {
		o.Engines = DefaultEngines()
	}
	if p.Version != ParamsVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrParamsVersion, p.Version, ParamsVersion)
	}
	if err := p.validate(); err != nil {
		return err
	}
	inHash := newHasher()
	in := &countingReader{r: io.TeeReader(uncompressed, inHash)}
	outHash := newHasher()
	out := &countingWriter{w: &ctxWriter{ctx: ctx, w: io.MultiWriter(w, outHash)}}

	var err error
	switch p.Format {
	case FormatNone:
		_, err = io.Copy(out, in)
	case FormatGzip:
		err = recompressGzip(p, in, out, &o)
	case FormatZstd:
		err = recompressZstd(p, in, out, &o)
	}
	if err != nil {
		// The engine may have failed because the input does not match
		// Params rather than because of a genuine engine problem (a
		// deflate/zstd writer can choke on wrong content well before EOF).
		// Drain the rest of the input so the digest is complete, and let a
		// confirmed mismatch take precedence over the engine's own error.
		io.Copy(io.Discard, in) // best effort; a drain error does not change what we report
		if got := digestOf(inHash, in.n); got != p.Uncompressed {
			return fmt.Errorf("%w: input is %s/%d, params expect %s/%d (engine reported: %v)", ErrInputMismatch, got.Blake3, got.Size, p.Uncompressed.Blake3, p.Uncompressed.Size, err)
		}
		return err
	}
	if got := digestOf(inHash, in.n); got != p.Uncompressed {
		return fmt.Errorf("%w: input is %s/%d, params expect %s/%d", ErrInputMismatch, got.Blake3, got.Size, p.Uncompressed.Blake3, p.Uncompressed.Size)
	}
	if got := digestOf(outHash, out.n); got != p.Compressed {
		return fmt.Errorf("%w: output is %s/%d, params expect %s/%d", ErrDigestMismatch, got.Blake3, got.Size, p.Compressed.Blake3, p.Compressed.Size)
	}
	return nil
}

func lookupEngine(p *Params, o *RecompressOptions) (engine.Engine, error) {
	e, ok := engine.ByName(o.Engines, p.Engine)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrEngineUnavailable, p.Engine)
	}
	if e.Format() != p.Format {
		return nil, fmt.Errorf("%w: %q produces %s, params are %s", ErrEngineUnavailable, p.Engine, e.Format(), p.Format)
	}
	if v := e.Version(); v != p.EngineVersion && !o.AllowVersionMismatch {
		return nil, fmt.Errorf("%w: %s is %s, params were made with %s", ErrEngineVersionMismatch, p.Engine, v, p.EngineVersion)
	}
	return e, nil
}

func recompressGzip(p *Params, in *countingReader, out io.Writer, o *RecompressOptions) error {
	e, err := lookupEngine(p, o)
	if err != nil {
		return err
	}
	de, ok := e.(engine.DeflateEngine)
	if !ok {
		return fmt.Errorf("%w: %q is not a %s engine", ErrEngineUnavailable, p.Engine, p.Format)
	}
	hdr, err := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64)
	if err != nil {
		return fmt.Errorf("%w: gzip header_b64: %v", ErrInvalidParams, err)
	}
	if _, err := out.Write(hdr); err != nil {
		return err
	}
	crc := crc32.NewIEEE()
	dw, err := de.NewWriter(out, p.Gzip.DeflateParams)
	if err != nil {
		return err
	}
	if _, err := engine.Feed(dw, io.TeeReader(in, crc)); err != nil {
		dw.Close()
		return err
	}
	if err := dw.Close(); err != nil {
		return err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc.Sum32())
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(in.n))
	_, err = out.Write(trailer[:])
	return err
}

func recompressZstd(p *Params, in io.Reader, out io.Writer, o *RecompressOptions) error {
	e, err := lookupEngine(p, o)
	if err != nil {
		return err
	}
	ze, ok := e.(engine.ZstdEngine)
	if !ok {
		return fmt.Errorf("%w: %q is not a %s engine", ErrEngineUnavailable, p.Engine, p.Format)
	}
	zw, err := ze.NewWriter(out, *p.Zstd, p.Uncompressed.Size)
	if err != nil {
		return err
	}
	if _, err := engine.Feed(zw, in); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}
