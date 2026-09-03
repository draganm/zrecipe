package compprysm

import "errors"

var (
	// ErrUnsupported reports an input the library recognises but does not
	// handle: multi-member gzip, multi-frame zstd, skippable frames,
	// dictionaries.
	ErrUnsupported = errors.New("compprysm: unsupported input")
	// ErrCorrupt reports an input that failed to decompress or verify.
	ErrCorrupt = errors.New("compprysm: corrupt input")
	// ErrNotReproducible reports that no candidate reproduced the input.
	ErrNotReproducible = errors.New("compprysm: not reproducible")
	// ErrInputMismatch reports uncompressed input that does not match Params.
	ErrInputMismatch = errors.New("compprysm: uncompressed input does not match params")
	// ErrDigestMismatch reports recompressed output that does not match Params.
	ErrDigestMismatch = errors.New("compprysm: recompressed output does not match params")
	// ErrEngineUnavailable reports an engine name that is not in the set.
	ErrEngineUnavailable = errors.New("compprysm: engine unavailable")
	// ErrEngineVersionMismatch reports an engine version different from Params.
	ErrEngineVersionMismatch = errors.New("compprysm: engine version mismatch")
	// ErrParamsVersion reports an unknown Params schema version.
	ErrParamsVersion = errors.New("compprysm: unsupported params version")
	// ErrInvalidParams reports Params that are internally inconsistent.
	ErrInvalidParams = errors.New("compprysm: invalid params")
)
