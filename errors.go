package zrecipe

import "errors"

var (
	// ErrUnsupported reports an input the library recognises but does not
	// handle: multi-member gzip, multi-frame zstd, skippable frames,
	// dictionaries.
	ErrUnsupported = errors.New("zrecipe: unsupported input")
	// ErrCorrupt reports an input that failed to decompress or verify.
	ErrCorrupt = errors.New("zrecipe: corrupt input")
	// ErrNotReproducible reports that no candidate reproduced the input.
	ErrNotReproducible = errors.New("zrecipe: not reproducible")
	// ErrInputMismatch reports uncompressed input that does not match Params.
	ErrInputMismatch = errors.New("zrecipe: uncompressed input does not match params")
	// ErrDigestMismatch reports recompressed output that does not match Params.
	ErrDigestMismatch = errors.New("zrecipe: recompressed output does not match params")
	// ErrEngineUnavailable reports an engine name that is not in the set.
	ErrEngineUnavailable = errors.New("zrecipe: engine unavailable")
	// ErrEngineVersionMismatch reports an engine version different from Params.
	ErrEngineVersionMismatch = errors.New("zrecipe: engine version mismatch")
	// ErrParamsVersion reports an unknown Params schema version.
	ErrParamsVersion = errors.New("zrecipe: unsupported params version")
	// ErrInvalidParams reports Params that are internally inconsistent.
	ErrInvalidParams = errors.New("zrecipe: invalid params")
)
