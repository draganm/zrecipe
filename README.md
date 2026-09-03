# comp-prysm

comp-prysm is a Go library that makes compressed files reproducible from
their uncompressed content: given a gzip or zstd file it decompresses once,
searches a set of real-world compression engines and parameter grids for the
combination that recreates the exact same compressed bytes, and records that
combination as a small JSON document alongside blake3 digests of both
streams; given the uncompressed content and that document it rebuilds the
original compressed file bit for bit. The intended use is storage systems
that want to keep only the uncompressed content — which deduplicates and
chunks well — while still being able to hand back the original compressed
artifact on demand.

## Install and build

comp-prysm ships as a Nix flake that provides the Go toolchain and the C
libraries the cgo engines link against (system zlib and zstd, plus `gzip`
and `pigz` for the wild-fixture tests). Enter the shell and build from
there:

```sh
nix develop
go build ./cmd/comp-prysm
```

Everything below assumes commands run inside `nix develop` (or
`nix develop --command ...` from outside it). The library also builds
without cgo, in which case only the four pure-Go engines, `gnu-gzip`,
`go-flate`, `klauspost-flate` and `klauspost-zstd`, are available and
`zlib`/`libzstd` are absent from `DefaultEngines()`.

## Library usage

The root package is `github.com/draganm/comp-prysm`. `Analyze` takes an
`io.ReadSeeker` over a compressed file, decompresses it once while hashing
both the compressed and uncompressed bytes, and returns `*Params`, which
records the engine, its version, and the parameters that reproduce the
file. `Recompress` takes those `*Params` and the uncompressed content and
streams the compressed file back out, verifying both the input and the
output against the digests in `Params`.

```go
ctx := context.Background()

in, err := os.Open("archive.tar.gz")
// ...
defer in.Close()

uncompressed, err := os.Create("archive.tar")
// ...
defer uncompressed.Close()

// Analyze decompresses once, hashes both streams, and finds an engine and
// parameter set that reproduces archive.tar.gz byte for byte from
// archive.tar. Store archive.tar and the params JSON; the .gz can now be
// discarded.
params, err := compprysm.Analyze(ctx, in, &compprysm.Options{Uncompressed: uncompressed})
// ...

paramsFile, err := os.Create("params.json")
// ...
err = params.Write(paramsFile)
paramsFile.Close()

// Later, rebuild the original .gz from archive.tar and params.json.
src, err := os.Open("archive.tar")
// ...
defer src.Close()

out, err := os.Create("rebuilt.tar.gz")
// ...
defer out.Close()

err = compprysm.Recompress(ctx, params, src, out, nil)
```

For an uncompressed input `Analyze` returns `Params` with `Format` set to
`none`, both digests equal, and no engine, so callers have one code path
for every input. `Options.Parallelism` (default `runtime.NumCPU()`) evaluates
candidates concurrently, but only takes effect when the reader also
implements `io.ReaderAt` (as `*os.File` does); otherwise the search runs
sequentially over the single `io.ReadSeeker`. Large inputs spool the
decompressed content to a temp file once they exceed `Options.MaxInMemory`
(default 64 MiB), so memory stays bounded regardless of input size —
`TestLargeInput` in this package exercises a 2 GiB input and stays well
under that bound (see Testing below).

`ReadParams` decodes and validates a `Params` document read back from JSON,
rejecting an unknown schema version with `ErrParamsVersion` and an
internally inconsistent document — malformed digest hex, or a `gzip`/`zstd`
section that does not match `format` — with `ErrInvalidParams`.

## CLI usage

`cmd/comp-prysm` wraps the library in four subcommands:

```sh
# Print gzip, zstd or none for a file.
comp-prysm detect archive.tar.gz

# Find parameters that reproduce a compressed file. Params JSON goes to
# stdout by default, or to --params; --uncompressed additionally writes the
# decompressed content. Flags must come before the positional <file>: this
# is a urfave/cli v2 limitation (it stops parsing flags at the first
# positional argument), not a choice made by this tool.
comp-prysm analyze --params params.json --uncompressed archive.tar archive.tar.gz

# Rebuild the compressed file from params and the uncompressed content.
# Writes to a temp file next to <out> and renames on success, so a failed
# run never leaves a partial file at the destination. Same flags-first rule.
comp-prysm recompress --params params.json archive.tar rebuilt.tar.gz

# List the engines compiled into this binary, with their format and version.
comp-prysm engines
```

Exit status is 0 on success and 1 on any error, with the error printed to
stderr. `recompress` also accepts `--allow-version-mismatch` to try an
engine whose recorded version differs from the one compiled into the
binary; `analyze` accepts `--parallelism` and `--temp-dir` to override the
defaults described above.

## Engines

| Name | Format | Binding | Version source |
|---|---|---|---|
| `gnu-gzip` | gzip | pure-Go port of GNU gzip's compressor | the ported release, `1.14` |
| `zlib` | gzip | cgo, system zlib | `zlibVersion()` |
| `pigz` | gzip | cgo, system zlib driven the way pigz does | `2.8+zlib` plus `zlibVersion()` |
| `libzstd` | zstd | cgo, system libzstd | `ZSTD_versionString()` |
| `go-flate` | gzip | stdlib `compress/flate` | `runtime.Version()` |
| `klauspost-flate` | gzip | `github.com/klauspost/compress/flate` | module version from `debug.ReadBuildInfo()` |
| `klauspost-zstd` | zstd | `github.com/klauspost/compress/zstd` | module version from `debug.ReadBuildInfo()` |

`gnu-gzip` is a line-by-line port of the compressor in GNU gzip 1.14
(`deflate.c`, `trees.c` and `bits.c`) and produces exactly the bytes the
`gzip` program writes when it compresses a regular file, at every level
and with `--rsyncable`. It is listed first in `DefaultEngines()` because
GNU gzip is the most common producer of gzip files in the wild, and because
a pure-Go engine keeps the resulting Params usable from a binary built
without cgo. The ported files are GPL-licensed; see License below.

`pigz` reproduces pigz, the parallel gzip. pigz compresses with zlib but
cuts the input into blocks (128 KiB by default), restarts the compressor on
every block primed with the previous 32 KiB, and byte-aligns each block
with empty deflate blocks, so a plain zlib stream matches its output only
for input that fits in one block. The engine drives the system zlib exactly
as pigz 2.8 does, covering both of pigz's code paths (`-p 1` keeps one
stream and flushes at the same boundaries; more threads reset per block),
`--independent`, `--rsyncable`, `-b` block sizes, and the `-H`/`-U`
strategies. zopfli (`-11`) is not covered. Its version string names both
the pigz release ported and the zlib linked, since the output depends on
both.

The three cgo engines are present only in binaries built with cgo enabled.
Against the versions pinned by this repository's flake, `zlib` reports
`1.3.2` and `libzstd` reports `1.5.7`; `klauspost-flate` and
`klauspost-zstd` both report the `github.com/klauspost/compress` module
version, `v1.20.0` at the time of writing. Build info carries no module
version inside a `go test` binary, so the klauspost engines report
`(devel)` there; `Recompress` treats that like any other version string.
`go-flate`'s version is whatever Go toolchain built the binary. `gnu-gzip`
reports the GNU gzip release it ports; that string only changes if a change
to the port alters its output.

**Version granularity.** `go-flate` is versioned by the full Go release
(`runtime.Version()`, e.g. `go1.22.3`), not just the major/minor line, so
even a Go *patch* release changes `engine_version` and makes `Recompress`
refuse a `Params` document made with a different patch release unless
`AllowVersionMismatch` (`--allow-version-mismatch` on the CLI) is set — the
digest check still decides whether the attempt actually reproduces the
file. The klauspost engines report `(devel)` not only from a `go test`
binary but from any build where `debug.ReadBuildInfo()` cannot resolve a
concrete version for the module, such as a Go workspace (`go.work`) build
that replaces it with a local checkout.

Different implementations produce different bytes at the same nominal
level, and libzstd's output changes between releases while zlib's has been
stable for years, so a binary reproduces zstd files only against the
libzstd version it is linked against — see Limitations below.

## Params JSON

`Params` is versioned (currently 1), carries blake3 digests and sizes of
both the compressed and uncompressed content, the winning engine's name and
version, and either a `gzip` or a `zstd` section with that engine's
parameters. Here is what `comp-prysm analyze` prints for a small file gzipped
by the system's `gzip -6`, reproduced by the `zlib` engine:

```json
{
  "version": 1,
  "format": "gzip",
  "compressed": {
    "blake3": "7ecf3d688fe675af5b076c00c4dd7b63a4a3af45df12bc3232d35ba842eb84e2",
    "size": 114
  },
  "uncompressed": {
    "blake3": "56f278db158c5413742c7209762b6dd4c0b776a6246cd29397f54e18f277b81c",
    "size": 8800
  },
  "engine": "zlib",
  "engine_version": "1.3.2",
  "gzip": {
    "header_b64": "H4sICNlmmWoAA3NhbXBsZS50eHQA",
    "level": 6,
    "strategy": "default",
    "window_bits": 15,
    "mem_level": 8
  }
}
```

`header_b64` is the gzip header captured verbatim, byte 0 up to the first
byte of the deflate stream, so mtime, filename, OS byte, XFL and every
optional field come back exact without needing to be parsed individually.
`strategy` is meaningful only for the `zlib` engine (`default`, `filtered`,
`huffman_only`, `rle` or `fixed`); the pure-Go engines omit it because they
have no equivalent knob. `rsyncable` records `--rsyncable` for `gnu-gzip`
and `pigz`. `block_size` (in KiB), `independent` and `single_thread` are
`pigz` only: its `-b`, `-i`, and the `-p 1` code path.

The same file compressed with the `zstd` CLI instead produces a `zstd`
section:

```json
{
  "version": 1,
  "format": "zstd",
  "compressed": {
    "blake3": "45cd33d739d17e4190228332f616794eca0fb92be4445f8eeb7851ac3694a9d4",
    "size": 67
  },
  "uncompressed": {
    "blake3": "56f278db158c5413742c7209762b6dd4c0b776a6246cd29397f54e18f277b81c",
    "size": 8800
  },
  "engine": "libzstd",
  "engine_version": "1.5.7",
  "zstd": {
    "level": 3,
    "checksum": true,
    "content_size": true,
    "pledged_size": true,
    "single_segment": true,
    "workers": 0,
    "end_with_data": true
  }
}
```

`window_log`, `long` and `encode_all` are omitted here because they are
zero-valued or false; they, along with the always-present `workers`, cover
long-distance matching mode, an explicit window size, and klauspost's
one-shot `EncodeAll` encoding path. `end_with_data` is an addition beyond
the original design. It does not record what the producer did — comp-prysm
has no way to observe that — but what the frame itself shows: whether the
last block is non-empty (`true`) or an explicit empty block trails the data
(`false`). A known-size producer such as the zstd CLI reading a file
typically yields the non-empty shape by passing its final chunk of input
together with the end directive; a producer that only learns end-of-input
later must call end separately once there is nothing left to flush,
leaving that trailing empty block. For input that is not aligned to zstd's
block size, though, there is always unflushed data buffered when end is
signalled, so the last block comes out non-empty regardless of which way
the producer called it — the two directives converge on the same frame,
and `end_with_data` reads `true` either way.

## Limitations

A binary reproduces zstd files made only by the exact libzstd version it is
linked against; that version is recorded in `engine_version` and, absent
`AllowVersionMismatch` (`--allow-version-mismatch` on the CLI),
`Recompress` refuses to even try an engine whose current version differs
from the recorded one. Passing that flag makes it try anyway — the digest
check still decides whether the attempt actually succeeded.

`Analyze` recognises but does not handle four kinds of input, all returned
as `ErrUnsupported`: multi-member gzip files, multi-frame zstd files, zstd
skippable frames, and zstd frames built against a dictionary.

`Analyze`'s zstd decoder is bounded only by the window size declared in the
frame header, up to zstd's own maximum of 2 GiB (`WithDecoderMaxWindow`).
An untrusted zstd input can therefore make decompression demand up to that
much memory for its window alone; callers decompressing files from
untrusted sources should account for this.

When the `klauspost-zstd` engine produces a single-segment frame (one
without a window descriptor) or uses its one-shot `EncodeAll` path, it must
buffer the entire uncompressed content in memory first; both are capped at
1 GiB of uncompressed size, above which those candidates are skipped rather
than exhausting memory.

`Recompress` does not roll back bytes already written to its output writer
on error, since arbitrary `io.Writer`s are not generally seekable or
truncatable; the CLI works around this itself by writing to a temporary
file next to the destination and renaming it into place only once
`Recompress` succeeds, and library callers that need the same safety should
do likewise.

Finally, not every real-world compressor is reproducible in this version.
A spike against this repository's flake-pinned tools (zstd CLI 1.5.7, GNU
gzip 1.14, pigz 2.8) found that the zstd CLI reproduces in all 96 tested
variants — levels 1 through 22, `--fast`, `-T0`/`-T1`/`-T4`,
`--single-thread`, `--no-check`, `--long`, from both stdin and a file. GNU
gzip reproduces at every level, with and without `--rsyncable`, through
the `gnu-gzip` engine; zlib alone matched it only at levels 8 and 9 on
plain text, because gzip ends deflate blocks with its own heuristic. One
caveat: `gnu-gzip` models gzip reading a regular file, where every
`read(2)` returns the full amount asked for. gzip reading from a pipe can
get short reads, which shift the point where its window slides; that
changes the output only when the last few hundred bytes of input fall in
the region where gzip stops matching, or when a final match runs past the
end of input into stale window bytes, and in those cases gzip's own
pipe output is timing-dependent. pigz is reproduced through the `pigz`
engine at every level, on both of its code paths, with `-i`, `-R` and `-b`;
zlib alone matched only input that fits in a single pigz block. Files
produced by zlib itself, Go's `compress/gzip`, and both klauspost engines
are reproduced.

## Testing

Besides the unit, round-trip and wild-fixture tests that run on every
`go test`, `TestLargeInput` in this package streams a synthetic 2 GiB gzip
file through `Analyze` and `Recompress` to confirm the spool and the search
keep memory bounded on inputs far larger than `MaxInMemory`. It is gated
behind an environment variable because it takes minutes and several
gigabytes of scratch disk:

```sh
COMP_PRYSM_LARGE=1 go test . -run LargeInput -v -timeout 30m
```

## License

comp-prysm is licensed under the GNU Affero General Public License,
version 3 or later; see `LICENSE`. The `engine/gnugzip` package contains
code ported from GNU gzip, which is licensed under the GNU General Public
License, version 3 or later; those files keep their upstream copyright
notices and `engine/gnugzip/COPYING` holds that license. Section 13 of
each license permits combining the two: the ported files remain under the
GPL, the rest of the project is under the AGPL, and the AGPL's
network-interaction terms apply to the AGPL-covered parts. Every other
dependency is under a permissive license (BSD-3-Clause, MIT or the zlib
license) that is compatible with both.

In practice this means a program that imports comp-prysm must itself be
distributed under AGPL-compatible terms, and one that offers it as a
network service must offer its source to the users of that service.

## Design and plan

The full design, including the search algorithm, the candidate grids for
each engine, and the spike results that shaped the limitations above, lives
at `docs/superpowers/specs/2026-09-03-comp-prysm-design.md`, with the GNU
gzip engine and the AGPL relicensing in
`docs/superpowers/specs/2026-09-03-gnu-gzip-engine-design.md` and the
pigz engine in `docs/superpowers/specs/2026-09-03-pigz-engine-design.md`. The
implementation plan that built this library task by task lives at
`docs/superpowers/plans/2026-09-03-comp-prysm.md`.
