# pgzip engine

Addendum to the zrecipe design of 2026-09-03, for issue #2. Layers of
Canonical's rock-built images (`ubuntu:latest` on Docker Hub among them)
are written by umoci with klauspost/pgzip v1.2.6 over klauspost/compress
v1.11.3, and zrecipe v0.2.0 reports them not reproducible: no engine
implements pgzip's block scheme, and the `klauspost-flate` engine links
klauspost/compress v1.20.0, whose encoder produces different bytes from
v1.11's at every level. This engine reproduces them.

## What pgzip does

pgzip's Writer buffers input into blocks of `defaultBlockSize`, 1 MiB
(`SetConcurrency` lets callers pick any block above `tailSize`, 16 KiB).
Each full block goes to a goroutine that takes a `flate.Writer` from a
pool, calls `ResetDict(dst, prevTail)` with the last 16 KiB of the
previous block (nil for the first block, and nil after a block of 16 KiB
or less, which only a mid-stream `Flush` can produce), writes the block
in one call, `Flush`es (a sync marker after every block), and, for the
block that `Close` submits, `Close`s the stream. `Close` always submits
the buffered remainder, so input that is an exact multiple of the block
size ends with an empty block: a sync marker and an empty final block.
Results are written out in block order. Neither the number of goroutines
nor the size of the caller's writes changes the output.

The header is Go's: OS byte 255, XFL 2 for level 9 and 4 for level 1,
otherwise 0, and the caller's `ModTime` truncated to 32 bits, which for
the zero `time.Time` is `0x886e0900` (`00 09 6e 88` on the wire). That
mtime is the signature by which pgzip 1.2.6 files can be told apart.

## Why an engine, and why a copy of flate

Emulating the scheme over the linked klauspost/compress v1.20.0 does not
match: the encoder changed in v1.11.13 and again afterwards. Emulating it
over v1.11.3 matches byte for byte (the issue reports the same for
v1.10.11, v1.11.0 and v1.11.7). Go cannot load two versions of one
module, so `engine/pgzip/flate` is a verbatim copy of the v1.11.3 flate
package (gofmt-reformatted, BSD license kept) under zrecipe's own import
path. Later klauspost generations that a real-world producer turns out to
need can be added the same way, as separate copies and separate versions.

## Engine

Name `pgzip`, format gzip, pure Go. Version
`1.2.6+klauspost-compress1.11.3`: the pgzip release ported plus the
klauspost/compress release copied, since the output depends on both.
Bump the second half if the copy is ever changed in a way that alters its
output.

Parameters: `level` (klauspost's -2 HuffmanOnly and 0 through 9; -1 is
rejected because klauspost maps it to 5 and the search lists 5) and
`block_size`, shared with pigz, in KiB, 0 meaning pgzip's 1024. Blocks of
16 KiB or less are rejected as pgzip rejects them. Every other deflate
field is rejected.

Candidates, two tiers:

1. the default block at every level: 5 first when XFL is 0 (klauspost's
   default, which pgzip callers mostly take), otherwise `LevelOrder`'s
   order, then HuffmanOnly;
2. other block sizes (128, 256, 512, 2048, 4096 KiB) at every level,
   pruned by the uncompressed size: a block larger than the input yields
   one block and the same bytes as every other such size, so only sizes
   up to the input are tried, plus one representative above it when the
   default is not above the input. Unlike pigz, a block exactly the size
   of the input is its own case, because it fills and Close then
   compresses an empty block after it.

`DefaultEngines` lists pgzip after `klauspost-flate`, before
`klauspost-zstd`.

## Implementation

The writer is pgzip's with the goroutines taken out: it appends into a
block buffer, compresses a block when the buffer fills, and on `Close`
compresses whatever is left, empty or not, then closes the deflate
stream. One `flate.Writer` is reused through `ResetDict` as pgzip's pool
reuses its writers; pgzip's own output would be nondeterministic if the
reset left state behind, and the reference comparison would show it. The
16 KiB tail is copied out of the block before the buffer is reused.

## Tests

- `engine/pgzip`: identity, candidate tiers and pruning, parameter
  rejection, the shared round-trip test over inputs that span several
  blocks (one an exact multiple), write-size independence, inflating,
  error propagation.
- `TestMatchesPgzip` builds `engine/pgzip/testdata/pgzipref`, a
  separate Go module pinning klauspost/pgzip v1.2.6 and klauspost/compress
  v1.11.3 (the main module cannot depend on both versions of
  klauspost/compress), runs it over the fixture set plus sizes around the
  tail and block boundaries at every level, with other block sizes, one
  block in flight and small writes, and compares the deflate payload byte
  for byte. It also checks the header signature above. It skips when `go`
  is not on PATH.
- `TestWildPgzip` in the root package runs the same reference through
  `Analyze` and `Recompress` and checks that the `pgzip` engine is the one
  that reproduces the file.
