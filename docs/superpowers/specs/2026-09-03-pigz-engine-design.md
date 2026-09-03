# pigz engine

Addendum to the comp-prysm design of 2026-09-03. The wild-fixture spike found
that pigz output on real-sized input matches no single-stream zlib candidate
at any level or worker count. This engine reproduces it.

## What pigz does

pigz compresses with zlib, but not as one stream. It reads the input in
blocks (128 KiB by default, `-b` in KiB, at least 32 KiB) and, with more
than one thread, hands each block to a worker that resets its deflate
stream, primes it with the last 32 KiB of input before the block
(`deflateSetDictionary`), compresses the block, and byte-aligns the result:
it closes the deflate block with `Z_BLOCK`, reads the number of pending
bits, and then either emits a sync marker (an empty stored block, when the
count is odd or in `--independent` mode) or primes one to three empty
static blocks of ten bits each until the stream is byte aligned. With
`--independent` it also adds a full flush. `--rsyncable` runs a 12-bit
rolling hash over the input and cuts blocks at every hit, grouping them
into jobs of at most one block size. The last block is finished with
`Z_FINISH`.

With one thread (`-p 1`, or the default on a one-CPU machine) pigz takes a
different code path: one deflate stream for the whole file, flushed at the
same block boundaries with the same byte-alignment logic, with a
`deflateReset` every block size of input in `--independent` mode. The two
paths produce different bytes because a reset-and-primed stream is not in
the same state as a continuing one.

Two details matter for byte-exactness at level 0, where zlib's stored-block
lengths depend on how much output space it is given: the parallel path
gives each job a buffer of `block + block/16 + 32 KiB` and grows it by
pigz's rule when it fills, and the single-thread path uses a buffer of one
block size per deflate call. Output does not depend on the thread count
once it is above one, nor on how the input is chunked, because pigz's
`readn` fills each block completely.

## Engine

Name `pigz`, format gzip, cgo only (it needs zlib's `deflateSetDictionary`,
`deflatePending` and `deflatePrime`). Version `2.8+zlib<zlibVersion()>`:
the output depends on both the pigz algorithm ported and the zlib linked.

`DeflateParams` gains three pigz-only fields: `BlockSize` (KiB, 0 means
128), `Independent` and `SingleThread`. pigz also honours `Rsyncable`
(shared with gnu-gzip) and `Strategy` for its `-H` (huffman_only) and
`-U` (rle) options. Every other deflate engine rejects the new fields.

Candidates, levels ordered by the XFL hint (pigz sets XFL the way gzip
does):

1. default block, parallel path, with dictionary: levels 0 through 9.
2. the same levels for `SingleThread`, `Independent` and `Rsyncable`.
3. other block sizes (32, 64, 256, 512, 1024, 2048, 4096 KiB), pruned by the
   uncompressed size: a block no smaller than the input yields one job and
   the same bytes as every other such block, so only sizes below the input
   are tried, plus the smallest size at or above it when the default is
   below it.
4. `huffman_only` and `rle` at levels 1 through 9.

zopfli (`-11`) is out of scope.

`DefaultEngines` lists pigz after zlib: for input that fits in one block
the two produce identical bytes, and zlib is the simpler record.

## Implementation

The engine ports `parallel_compress` (minus threads), `compress_thread`,
`single_compress` and `deflate_engine` from pigz 2.8 literally, including
the input-buffer juggling that decides where jobs end at end of input,
which is where a derived "cut at the last hash hit" rule goes wrong. The
port pulls input through `readn`, so it runs in a goroutine fed by an
`io.Pipe`; `Write` pushes bytes, `Close` closes the pipe and waits. A write
error on the destination stops the goroutine and surfaces on the next
`Write` or on `Close`.

pigz recycles output buffers between jobs, so a buffer grown by one job
could serve a later one with the larger size; that would only happen if a
job's output exceeded its buffer, which no level produces. The port gives
every job the pool size.

## Tests

- `engine/pigz`: identity, candidate tiers and pruning, parameter rejection,
  the shared round-trip test on a representative parameter subset, error
  propagation.
- `TestMatchesPigz` runs the flake's pigz on temp files over the fixture set
  plus sizes around block boundaries, with both code paths, all levels at
  the default settings, and `-i`, `-R`, `-b` variants at several levels,
  and compares the deflate payload byte for byte. It skips when pigz is not
  on PATH.
- `wild_test.go`: the pigz skips are removed and the variants extended.
