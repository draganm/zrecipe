package zrecipe

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// ParamsVersion is the schema version written by this package.
const ParamsVersion = 1

// Digest identifies content by blake3 hash and size.
type Digest struct {
	Blake3 string `json:"blake3"` // 64 hex characters
	Size   int64  `json:"size"`
}

// Params records how to rebuild a compressed file from its content.
type Params struct {
	Version       int         `json:"version"`
	Format        Format      `json:"format"`
	Compressed    Digest      `json:"compressed"`
	Uncompressed  Digest      `json:"uncompressed"`
	Engine        string      `json:"engine,omitempty"`
	EngineVersion string      `json:"engine_version,omitempty"`
	Gzip          *GzipParams `json:"gzip,omitempty"`
	Zstd          *ZstdParams `json:"zstd,omitempty"`
}

// Write encodes p as indented JSON.
func (p *Params) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// ReadParams decodes and validates a Params document.
func ReadParams(r io.Reader) (*Params, error) {
	var p Params
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("zrecipe: decode params: %w", err)
	}
	if p.Version != ParamsVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrParamsVersion, p.Version, ParamsVersion)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Params) validate() error {
	for name, d := range map[string]Digest{"compressed": p.Compressed, "uncompressed": p.Uncompressed} {
		if b, err := hex.DecodeString(d.Blake3); err != nil || len(b) != 32 {
			return fmt.Errorf("%w: %s blake3 must be 64 hex characters", ErrInvalidParams, name)
		}
		if d.Size < 0 {
			return fmt.Errorf("%w: %s size is negative", ErrInvalidParams, name)
		}
	}
	switch p.Format {
	case FormatNone:
		if p.Engine != "" || p.Gzip != nil || p.Zstd != nil {
			return fmt.Errorf("%w: format none must not carry an engine or parameters", ErrInvalidParams)
		}
	case FormatGzip:
		if p.Engine == "" || p.Gzip == nil || p.Zstd != nil {
			return fmt.Errorf("%w: gzip needs engine and gzip section only", ErrInvalidParams)
		}
		if _, err := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64); err != nil {
			return fmt.Errorf("%w: gzip header_b64: %v", ErrInvalidParams, err)
		}
	case FormatZstd:
		if p.Engine == "" || p.Zstd == nil || p.Gzip != nil {
			return fmt.Errorf("%w: zstd needs engine and zstd section only", ErrInvalidParams)
		}
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidParams, p.Format)
	}
	return nil
}
