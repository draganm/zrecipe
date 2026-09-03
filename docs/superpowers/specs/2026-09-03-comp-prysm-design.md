# comp-prysm design

Date: 2026-09-03
Status: approved design, pending implementation plan

## Purpose

comp-prysm is a Go library that makes compressed files reproducible from their
uncompressed content. Given an `io.ReadSeeker` it:

1. Detects whether the input is gzip, zstd, or uncompressed.
2. For a compressed input, finds an engine and parameter set that re-creates the
   exact same compressed bytes from the uncompressed content.
3. Records those parameters, together with blake3 digests of the compressed and
   uncompressed content, as a JSON document.
4. Given the JSON document and the uncompressed content, re-creates the
   compressed file and verifies it against the recorded blake3 digest.

The intended use is storage systems that want to keep only the uncompressed
content, which deduplicates and chunks well, while still being able to hand back
the original compressed artifact bit for bit.

## Goals

- Reproduce files from the wild: GNU gzip, zlib, pigz, the zstd CLI, libzstd,
  and files made by Go tooling such as Docker, containerd and BuildKit.
- Handle inputs of any size, including multi-gigabyte files, with bounded
  memory.
- Fail loudly and precisely when a file cannot be reproduced.
- Make adding a new engine or a new parameter a local change.

## Non-goals for v1

- Reproducing a file when no engine matches. There is no diff or correction
  stream fallback. Analyze returns `ErrNotReproducible`.
- Multi-member gzip files, multi-frame zstd files, zstd skippable frames and
  zstd dictionaries. These are rejected with `ErrUnsupported`.
- Supporting more than one libzstd version in a single binary.
- Formats other than gzip and zstd.

## Key constraint: engine identity and version

Different implementations produce different bytes at the same level. Go's
stdlib deflate, klauspost's deflate and zlib all differ. klauspost's zstd and
libzstd differ. libzstd's output also changes between releases, while zlib's
deflate output has been stable for many years.

Therefore the engine name and engine version are part of the recorded
parameters. A binary reproduces zstd files made by the libzstd version it is
linked against. The pinned nixpkgs provides zlib 1.3.2 and zstd 1.5.7.

## Package layout

No `internal` packages. Every package is importable so that third parties can
add engines or reuse the search.

| Package | Purpose |
|---|---|
| `github.com/draganm/comp-prysm` (package `compprysm`) | Public API: Detect, Analyze, Recompress, Params, errors, DefaultEngines |
| `.../format` | Magic-byte detection, gzip header parsing, zstd frame header parsing |
| `.../engine` | Engine interfaces, parameter types, engine set |
| `.../engine/zlib` | cgo engine over system zlib, raw deflate |
| `.../engine/libzstd` | cgo engine over system libzstd |
| `.../engine/goflate` | stdlib `compress/flate` engine |
| `.../engine/kpflate` | `github.com/klauspost/compress/flate` engine |
| `.../engine/kpzstd` | `github.com/klauspost/compress/zstd` engine |
| `.../search` | Spool, compare writer, candidate evaluation |
| `.../cmd/comp-prysm` | CLI built on `github.com/urfave/cli/v2` |

Parameter types live in `engine` because engines consume them and the root
package imports the engines. The root package re-exports them with type
aliases so callers only need the root import.

The cgo engines are guarded with `//go:build cgo`. The root package assembles
`DefaultEngines()` from explicit imports, with the cgo engines added by a
cgo-tagged file. Without cgo the library still builds with the three pure-Go
engines. There is no `init()` registration.

## Public API

```go
package compprysm

type Format = engine.Format // "none", "gzip", "zstd"

const (
    FormatNone = engine.FormatNone
    FormatGzip = engine.FormatGzip
    FormatZstd = engine.FormatZstd
)

// Detect reads the magic bytes and seeks r back to its start. An input shorter
// than any magic sequence is FormatNone.
func Detect(r io.ReadSeeker) (Format, error)

type Options struct {
    TempDir      string          // spool directory, default os.TempDir()
    MaxInMemory  int64           // spool in memory below this size, default 64 MiB
    Parallelism  int             // candidate workers, default runtime.NumCPU()
    Uncompressed io.Writer       // optional: receives the decompressed content
    Engines      []engine.Engine // default DefaultEngines()
}

// Analyze detects the format, decompresses once, hashes both streams, and
// searches for an engine and parameters that reproduce r exactly.
func Analyze(ctx context.Context, r io.ReadSeeker, opts *Options) (*Params, error)

type RecompressOptions struct {
    Engines              []engine.Engine // default DefaultEngines()
    AllowVersionMismatch bool            // try even if engine versions differ
}

// Recompress rebuilds the compressed file from uncompressed input and streams
// it to w. It verifies the input against p.Uncompressed and the output against
// p.Compressed. Bytes already written to w are not rolled back on error;
// callers should write to a temporary file and rename on success.
func Recompress(ctx context.Context, p *Params, uncompressed io.Reader, w io.Writer, opts *RecompressOptions) error

func DefaultEngines() []engine.Engine

func (p *Params) Write(w io.Writer) error       // indented JSON
func ReadParams(r io.Reader) (*Params, error)   // validates schema version
```

`Parallelism` greater than one takes effect only when `r` also implements
`io.ReaderAt`, which `*os.File` does. Otherwise the search runs sequentially on
the single `ReadSeeker`.

For an uncompressed input Analyze returns a `Params` with `Format` set to
`none`, both digests equal, and no engine. If `Uncompressed` is set the content
is copied to it. Callers therefore have one flow for every input.

## Params

```go
type Digest struct {
    Blake3 string `json:"blake3"` // 64 hex characters
    Size   int64  `json:"size"`
}

type Params struct {
    Version       int                `json:"version"` // schema version, currently 1
    Format        Format             `json:"format"`
    Compressed    Digest             `json:"compressed"`
    Uncompressed  Digest             `json:"uncompressed"`
    Engine        string             `json:"engine,omitempty"`
    EngineVersion string             `json:"engine_version,omitempty"`
    Gzip          *engine.GzipParams `json:"gzip,omitempty"`
    Zstd          *engine.ZstdParams `json:"zstd,omitempty"`
}
```

Example for a gzip file made by zlib, digests abbreviated:

```json
{
  "version": 1,
  "format": "gzip",
  "compressed":   { "blake3": "3f1c…", "size": 123456 },
  "uncompressed": { "blake3": "9a0b…", "size": 987654 },
  "engine": "zlib",
  "engine_version": "1.3.2",
  "gzip": {
    "header_b64": "H4sIAAAAAAAAAw==",
    "level": 6,
    "strategy": "default",
    "window_bits": 15,
    "mem_level": 8
  }
}
```

Example for a zstd file made by the zstd CLI from a file, digests abbreviated:

```json
{
  "version": 1,
  "format": "zstd",
  "compressed":   { "blake3": "…", "size": 4242 },
  "uncompressed": { "blake3": "…", "size": 65536 },
  "engine": "libzstd",
  "engine_version": "1.5.7",
  "zstd": {
    "level": 3,
    "checksum": true,
    "content_size": true,
    "pledged_size": true,
    "single_segment": true,
    "workers": 0
  }
}
```

Fields an engine does not use are omitted. `ReadParams` returns
`ErrParamsVersion` for an unknown `version`.

## Engine model

Engines produce only the compressed payload.

For gzip an engine produces a raw deflate stream. The library captures the
original gzip header verbatim, from byte 0 up to the first byte of the deflate
stream, including any FEXTRA, FNAME, FCOMMENT and FHCRC fields, and stores it
base64 encoded in `header_b64`. The trailer is CRC32 (IEEE) of the uncompressed
content and its size modulo 2^32, both little endian, computed during the first
pass. Reproduction is header bytes, deflate stream, trailer. This makes mtime,
filename, OS byte, XFL and every optional field exact without parsing risk.

For zstd the engine writes the whole frame, because every frame header field is
a function of the encoder parameters.

```go
package engine

type Engine interface {
    Name() string    // stable identifier stored in Params, e.g. "zlib"
    Version() string // stored in Params, see table below
    Format() Format  // gzip or zstd
}

type DeflateParams struct {
    Level      int    `json:"level"`
    Strategy   string `json:"strategy,omitempty"`    // zlib only: default, filtered, huffman_only, rle, fixed
    WindowBits int    `json:"window_bits,omitempty"` // zlib only: 9..15
    MemLevel   int    `json:"mem_level,omitempty"`   // zlib only: 1..9
}

type GzipParams struct {
    HeaderB64 string `json:"header_b64"`
    DeflateParams
}

type DeflateEngine interface {
    Engine
    // Candidates returns parameter sets grouped by tier, most likely first.
    Candidates(h format.GzipHeader, uncompressedSize int64) [][]DeflateParams
    NewWriter(w io.Writer, p DeflateParams) (io.WriteCloser, error)
}

type ZstdParams struct {
    Level         int  `json:"level"`
    WindowLog     int  `json:"window_log,omitempty"` // 0 means engine default for the level
    Checksum      bool `json:"checksum"`
    ContentSize   bool `json:"content_size"`   // frame header carries the content size
    PledgedSize   bool `json:"pledged_size"`   // encoder knew the source size
    SingleSegment bool `json:"single_segment"`
    Workers       int  `json:"workers"`        // 0: single-thread path, 1: job-based path
    Long          bool `json:"long,omitempty"` // long distance matching
}

type ZstdEngine interface {
    Engine
    Candidates(h format.ZstdFrameHeader, uncompressedSize int64) [][]ZstdParams
    NewWriter(w io.Writer, p ZstdParams, uncompressedSize int64) (io.WriteCloser, error)
}
```

`engine.Format` is a string type with the constants `FormatNone`, `FormatGzip`
and `FormatZstd`. The root package re-exports the type and constants with
aliases.

### Engines

| Name | Format | Binding | Version() source |
|---|---|---|---|
| `zlib` | gzip | cgo, system zlib | `zlibVersion()` |
| `libzstd` | zstd | cgo, system libzstd | `ZSTD_versionString()` |
| `go-flate` | gzip | stdlib | `runtime.Version()` |
| `klauspost-flate` | gzip | Go module | module version from `debug.ReadBuildInfo()` |
| `klauspost-zstd` | zstd | Go module | module version from `debug.ReadBuildInfo()` |

If build info has no version for the klauspost module, for example when built
from a workspace, the version is `(devel)`, and Recompress treats it like any
other version string.

### Candidate grids

Tier 1 of every engine is tried before tier 2 of any engine, so the common cases
resolve in a handful of candidates. Within a tier the order is by prior
likelihood.

**zlib.** Tier 1: levels 0 to 9, default strategy, window bits 15, memory level
8. Order from the XFL hint in the header: XFL 2 puts level 9 first, XFL 4 puts
level 1 first, otherwise level 6 first and then outward. Tier 2: levels crossed
with strategies filtered, huffman_only, rle and fixed. Tier 3: levels crossed
with memory level 1 to 9 and window bits 9 to 15. Both change block boundaries
and match selection, so both matter for exactness.

**go-flate.** Levels 0 to 9 and Huffman only, one tier, same XFL ordering.

**klauspost-flate.** Levels 0 to 9 and Huffman only, one tier, same XFL
ordering.

**libzstd.** The frame header fixes the checksum flag, the content size flag,
the single segment flag and the window log. These are read, not searched. Tier
1: levels 1 to 19, crossed with workers 0 or 1, crossed with pledged size true
or false when the header has no content size, or pledged size true only when it
does. Window log left at the engine default. Tier 2: levels 20 to 22, fast
levels -1 to -7, long mode when the header window log is 27 or more, and an
explicit window log equal to the header value. Job size, overlap log, strategy
and hash parameters stay at defaults in v1.

Workers 1 selects libzstd's job-based path. Its output is expected to be
independent of the actual number of worker threads. This is verified by a test
before the engine is relied on.

**klauspost-zstd.** Levels fastest, default, better and best, with the window
size and checksum flag taken from the header, encoder concurrency fixed at 1,
one tier. Determinism with respect to concurrency is verified by a test.

## Search

### Pass 1, one streaming read

1. Detect the format from the magic bytes. Unknown magic means `FormatNone`.
2. Parse the header. Gzip: record the raw header bytes. Zstd: parse the frame
   header. A skippable frame magic or a non-zero dictionary ID returns
   `ErrUnsupported`.
3. For zstd, find the frame's extent by walking the block headers, which needs
   no decoding: each block header carries a last-block flag, a type and a size.
   The decoder is then bounded to exactly that frame. For gzip the stdlib
   reader is put in single-member mode, so it stops at the first trailer.
4. Decompress once. While streaming: hash the compressed bytes with blake3,
   hash the uncompressed bytes with blake3, tee the uncompressed bytes to
   `Options.Uncompressed` if set, and write them to the spool.
5. After the gzip member or the zstd frame ends, the input must be at EOF.
   Trailing bytes mean a multi-member or multi-frame file and return
   `ErrUnsupported`. Any decompression error, including a gzip trailer that
   does not verify, returns `ErrCorrupt`.

### Spool

The spool holds the uncompressed content for the duration of the search. The
uncompressed size is not known before decompression, so the spool starts as a
byte slice and, once it would exceed `MaxInMemory`, spills into a temp file in
`TempDir` that is unlinked immediately after creation, so a crash leaves nothing
behind. Both forms provide independent readers via `io.ReaderAt`.

### Candidate list

Each engine matching the detected format returns its tiers. The search
concatenates all engines' tier 1 lists, then all tier 2 lists, and so on, into
one ordered list.

### Evaluation of one candidate

1. Open a reader over the compressed input positioned just after the header.
   With `io.ReaderAt` this is a `SectionReader`. Otherwise it is the
   `ReadSeeker` after a seek.
2. Open a reader over the spool.
3. Create a compare writer wrapping the compressed reader. On every `Write` it
   reads the same number of bytes from the reference and compares. The first
   differing byte, or the reference ending early, returns `errMismatch`. The
   engine's writer propagates that error and the candidate is abandoned.
4. Create the engine writer over the compare writer and copy the spool into it.
5. On `Close`, for gzip, write the trailer through the same compare writer.
6. Require the reference reader to be at EOF. Extra reference bytes mean a
   mismatch.

A wrong candidate typically dies within its first few kilobytes of output, so a
losing candidate costs a few kilobytes of compression and a few hundred
kilobytes of spool reads.

### Outcome

The first candidate that survives wins, and Analyze fills `Params` from the
engine name, engine version and the candidate's parameters. If the list is
exhausted Analyze returns `ErrNotReproducible` wrapping the number of
candidates tried.

### Parallelism

When the input implements `io.ReaderAt`, a pool of `Parallelism` workers pulls
candidates off the ordered list. Two parameter sets can produce identical
bytes, so to keep the result deterministic regardless of worker count the
winner is the earliest candidate in list order that succeeds: when a candidate
succeeds, unstarted candidates are skipped and running candidates later in the
order are cancelled through a derived context, while running candidates earlier
in the order are allowed to finish and replace the winner if they succeed.
Each worker holds one engine working set; libzstd at level 19 uses a few
hundred megabytes, so callers near a memory limit lower `Parallelism`.

Without `io.ReaderAt` the search is sequential.

### Cancellation

The context is checked between candidates and inside the compare writer, so a
cancelled search stops within one write.

## Recompress

1. Validate `Params`: known format, schema version 1, exactly one of `Gzip` or
   `Zstd` set for a compressed format, digests well formed.
2. Find the engine by name in the engine set. Missing: `ErrEngineUnavailable`.
3. Compare versions. Different: `ErrEngineVersionMismatch` carrying both
   versions, unless `AllowVersionMismatch` is set.
4. Stream: read `uncompressed` through a blake3 hasher into the engine writer;
   the engine writer writes into a blake3 hasher that tees to `w`. For gzip the
   library writes the decoded header before the payload and the trailer after
   it. For `FormatNone` the content is copied through unchanged.
5. On completion compare the input digest and size to `p.Uncompressed`. Different:
   `ErrInputMismatch`. Then compare the output digest and size to
   `p.Compressed`. Different: `ErrDigestMismatch`.

The input check is reported first so a caller can tell "wrong file" from
"engine drift".

## Errors

Package-level sentinels in the root package, matched with `errors.Is` and
wrapped with detail:

| Error | Meaning |
|---|---|
| `ErrUnsupported` | Multi-member gzip, multi-frame zstd, skippable frame, dictionary |
| `ErrCorrupt` | Decompression failed or trailer did not verify |
| `ErrNotReproducible` | No candidate reproduced the file; wraps the count tried |
| `ErrInputMismatch` | Uncompressed input digest or size differs from Params |
| `ErrDigestMismatch` | Recompressed output digest or size differs from Params |
| `ErrEngineUnavailable` | Engine named in Params is not in the engine set |
| `ErrEngineVersionMismatch` | Engine version differs from Params |
| `ErrParamsVersion` | Unknown Params schema version |

## CLI

`cmd/comp-prysm`, built with `github.com/urfave/cli/v2`. Exit status 0 on
success, 1 on any error, with the error printed to stderr.

```
comp-prysm detect <file>
    Prints gzip, zstd or none.

comp-prysm analyze <file> [--params <out.json>] [--uncompressed <out>] [--parallelism N] [--temp-dir DIR]
    Runs Analyze. Writes the Params JSON to --params, default stdout.
    Writes the uncompressed content to --uncompressed if given.

comp-prysm recompress --params <p.json> <uncompressed> <out> [--allow-version-mismatch]
    Runs Recompress. Writes to a temp file next to <out> and renames on success.

comp-prysm engines
    Lists engine names, formats and versions in the default set.
```

## Testing

**Fixtures.** Sample inputs with different character: English text, JSON,
zeros, random bytes, a mixed tar-like blob, and an empty file. Sizes from 0
bytes to a few megabytes so multiple deflate and zstd blocks occur. Generated
deterministically in test code rather than checked in.

**Round-trip property.** For every engine and every candidate in its grid:
compress a fixture, Analyze, Recompress, assert byte equality. Runs on every
`go test`.

**Wild fixtures.** Tests invoke the real tools from the dev shell: `gzip -1`
to `-9`, `gzip -n`, `gzip` with a filename, `zstd -1` to `-19`, `zstd -T0`,
`zstd --long`, `zstd --no-check`, `zstd` from stdin and from a file, and `pigz`
at several levels. Each output goes through Analyze and Recompress. A test
skips with a clear message when its tool is not on PATH so `go test` passes
outside the flake.

**Unit tests.** Magic detection, gzip header parsing with every optional field,
zstd frame header parsing, compare writer early abort and exact-EOF rule, spool
switching between memory and temp file, Params JSON golden file, schema version
rejection.

**Negative tests.** Concatenated gzip members, two zstd frames, a zstd file
with a dictionary ID, a truncated file, a corrupted trailer, Recompress with the
wrong input, Recompress with an unknown engine, Recompress with a tampered
engine version.

**Determinism tests.** libzstd with workers 1 versus workers 4 produces the same
bytes. klauspost-zstd with concurrency 1 versus 4 produces the same bytes.

**Large input.** A 2 GiB synthetic input, gated behind an environment variable,
confirms bounded memory and the temp file spool.

## Flake

The dev shell adds `zlib`, `zstd`, `pkg-config`, `gzip` and `pigz` to
`packages`. cgo uses the C compiler that `mkShell` already provides.
`hardeningDisable = ["all"]` stays.

## Risks settled by early spikes

Two facts are unknown and are resolved by running the wild-fixture tests as
the first step of engine work, before more engines are built on the answer:

1. **GNU gzip versus zlib.** Both descend from the same deflate code and are
   expected to produce identical output at each level. If they differ, GNU gzip
   needs its own engine. Its only source is GPL C code, which is a licensing
   decision for the project owner, so the spec does not commit to it.
2. **pigz.** pigz compresses in independent blocks primed with the preceding
   32 KiB as a dictionary. If it can be reproduced with zlib primitives it
   becomes a `pigz` engine with a block size parameter. If not, pigz files are
   `ErrNotReproducible` in v1.

The engine interface and candidate tiers are designed so either engine slots in
without touching the search.

## Future work, not in v1

- pigz and GNU gzip engines if the spikes require them.
- Analytical parameter inference from the bitstream as a ranker in front of the
  candidate list, if profiling shows the search is the bottleneck.
- Multiple libzstd versions via vendored, symbol-renamed copies.
- Multi-member and multi-frame inputs.
