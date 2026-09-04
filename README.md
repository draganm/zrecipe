# zrecipe

zrecipe is a Go library that makes compressed files reproducible from
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

zrecipe ships as a Nix flake that provides the Go toolchain and the C
libraries the cgo engines link against (system zlib and zstd, plus `gzip`
and `pigz` for the wild-fixture tests). Enter the shell and build from
there:

```sh
nix develop
go build ./cmd/zrecipe
```

Everything below assumes commands run inside `nix develop` (or
`nix develop --command ...` from outside it). The library also builds
without cgo, in which case only the five pure-Go engines, `gnu-gzip`,
`go-flate`, `klauspost-flate`, `pgzip` and `klauspost-zstd`, are available
and `zlib`, `pigz` and `libzstd` are absent from `DefaultEngines()`.

## Library usage

The root package is `github.com/draganm/zrecipe`. `Analyze` takes an
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
params, err := zrecipe.Analyze(ctx, in, &zrecipe.Options{Uncompressed: uncompressed})
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

err = zrecipe.Recompress(ctx, params, src, out, nil)
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

`cmd/zrecipe` wraps the library in four subcommands:

```sh
# Print gzip, zstd or none for a file.
zrecipe detect archive.tar.gz

# Find parameters that reproduce a compressed file. Params JSON goes to
# stdout by default, or to --params; --uncompressed additionally writes the
# decompressed content. Flags must come before the positional <file>: this
# is a urfave/cli v2 limitation (it stops parsing flags at the first
# positional argument), not a choice made by this tool.
zrecipe analyze --params params.json --uncompressed archive.tar archive.tar.gz

# Rebuild the compressed file from params and the uncompressed content.
# Writes to a temp file next to <out> and renames on success, so a failed
# run never leaves a partial file at the destination. Same flags-first rule.
zrecipe recompress --params params.json archive.tar rebuilt.tar.gz

# List the engines compiled into this binary, with their format and version.
zrecipe engines
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
| `pgzip` | gzip | pure-Go port of klauspost/pgzip's writer over a copy of `klauspost/compress/flate` v1.11.3 | the ported releases, `1.2.6+klauspost-compress1.11.3` |
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
strategies. zopfli (`-11`) is not covered. Like pigz, the engine compresses
the blocks of the parallel path concurrently, on `Engine.Workers` zlib
streams (GOMAXPROCS by default); the output does not depend on the count.
Its version string names both the pigz release ported and the zlib linked,
since the output depends on both.

`pgzip` reproduces klauspost/pgzip, the parallel gzip written in Go that
umoci compresses layers with, and through umoci rockcraft: every Canonical
rock on Docker Hub, `ubuntu` included. pgzip cuts the input into 1 MiB
blocks, compresses each with `klauspost/compress/flate` primed with the
last 16 KiB of the block before it, sync-flushes after every block, and
closes the stream after the last block, which it compresses even when
empty. Those bytes depend on the klauspost/compress generation as much as
on the scheme, and umoci pins v1.11.3, whose encoder differs from the
v1.20.0 the `klauspost-flate` engine links. Go cannot load two versions of
one module, so the engine drives a verbatim copy of that release's flate
package (`engine/pgzip/flate`, BSD-licensed) and its version string names
both the pgzip release ported and the klauspost/compress release copied.
`block_size` covers callers of `SetConcurrency`. pgzip files are easy to
recognise: OS byte 255 and, unless the producer set a modification time,
an mtime of `0x886e0900` (the zero `time.Time` truncated to 32 bits),
which the header captured in Params carries verbatim.

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

**Write-size independence.** An engine's output must depend only on the
content and the parameters, never on how the content was split across
`Write` calls: `Analyze` verifies a candidate from its spool while
`Recompress` rebuilds from whatever reader the caller passes. Most engines
have this property by construction. `zlib` at level 0 does not — zlib sizes
each stored block by the input one `deflate()` call can see, so 32 KiB
writes gave 32768-byte blocks where one large write gave maximal 65535-byte
ones — and neither does `libzstd` with `end_with_data`, which hands whatever
it holds back to `ZSTD_e_end`. Both engines therefore batch their input
internally (64 KiB for zlib, libzstd's own stream input size for libzstd),
and on top of that the search and `Recompress` both feed every engine
through `engine.Feed`, in fixed 32 KiB writes, so a candidate is always
verified under exactly the write shape later used to rebuild the file.

## Params JSON

`Params` is versioned (currently 1), carries blake3 digests and sizes of
both the compressed and uncompressed content, the winning engine's name and
version, and either a `gzip` or a `zstd` section with that engine's
parameters. Here is what `zrecipe analyze` prints for a small file gzipped
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
and `pigz`. `block_size` (in KiB) is the block the input is cut into for
`pigz` (`-b`, default 128) and `pgzip` (`SetConcurrency`, default 1024);
`independent` and `single_thread` are `pigz` only: its `-i` and the `-p 1`
code path.

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
the original design. It does not record what the producer did — zrecipe
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
are reproduced. klauspost/pgzip over klauspost/compress v1.11.3, the pair
umoci and rockcraft ship, is reproduced through the `pgzip` engine at
every level, block size and thread count; pgzip over other klauspost
generations is not, since the encoder changed in v1.11.13 and again
later, and those generations would each need their own copy of the flate
package.

## Testing

Besides the unit, round-trip and wild-fixture tests that run on every
`go test`, `TestLargeInput` in this package streams a synthetic 2 GiB gzip
file through `Analyze` and `Recompress` to confirm the spool and the search
keep memory bounded on inputs far larger than `MaxInMemory`. It is gated
behind an environment variable because it takes minutes and several
gigabytes of scratch disk:

```sh
ZRECIPE_LARGE=1 go test . -run LargeInput -v -timeout 30m
```

## License

zrecipe is licensed under the GNU Affero General Public License,
version 3 or later; see `LICENSE`. The `engine/gnugzip` package contains
code ported from GNU gzip, which is licensed under the GNU General Public
License, version 3 or later; those files keep their upstream copyright
notices and `engine/gnugzip/COPYING` holds that license. Section 13 of
each license permits combining the two: the ported files remain under the
GPL, the rest of the project is under the AGPL, and the AGPL's
network-interaction terms apply to the AGPL-covered parts. The
`engine/pgzip/flate` package is a copy of `github.com/klauspost/compress`'s
flate package at v1.11.3 under its BSD-3-Clause license, kept in
`engine/pgzip/flate/LICENSE`. Every other dependency is under a permissive
license (BSD-3-Clause, MIT or the zlib license) that is compatible with
both.

In practice this means a program that imports zrecipe must itself be
distributed under AGPL-compatible terms, and one that offers it as a
network service must offer its source to the users of that service.

## Design and plan

The full design, including the search algorithm, the candidate grids for
each engine, and the spike results that shaped the limitations above, lives
at `docs/superpowers/specs/2026-09-03-zrecipe-design.md`, with the GNU
gzip engine and the AGPL relicensing in
`docs/superpowers/specs/2026-09-03-gnu-gzip-engine-design.md`, the
pigz engine in `docs/superpowers/specs/2026-09-03-pigz-engine-design.md`
and the pgzip engine in
`docs/superpowers/specs/2026-09-04-pgzip-engine-design.md`. The
implementation plan that built this library task by task lives at
`docs/superpowers/plans/2026-09-03-zrecipe.md`.
