# GNU gzip engine and AGPL relicensing

Addendum to the zrecipe design of 2026-09-03. That spec left GNU gzip as an
open question: its deflate implementation is GPL C code, so an engine that
reproduces it byte for byte is a licensing decision for the project owner.
The wild-fixture spike (plan Task 15) then showed that zlib reproduces GNU
gzip only at levels 8 and 9 on plain text; every other level, and every
level on mixed content, uses a block-flush heuristic that no zlib parameter
models. The owner has decided: the project becomes AGPL-3.0-or-later and
gains a pure-Go port of GNU gzip's compressor.

## Licensing

- The repository is licensed AGPL-3.0-or-later. `LICENSE` at the root holds
  the license text.
- `engine/gnugzip` contains code ported from GNU gzip 1.14 (`deflate.c`,
  `trees.c`, `bits.c`). Those files keep their upstream copyright notices
  and remain GPL-3.0-or-later; `engine/gnugzip/COPYING` holds that text.
- Section 13 of each license permits combining an AGPL-3.0 work with a
  GPL-3.0 work. The combined program is distributed under both: the ported
  files under GPL-3.0-or-later, everything else under AGPL-3.0-or-later,
  with the AGPL network clause applying to the AGPL-covered parts.
- All existing dependencies are permissive (BSD-3-Clause, MIT, zlib) and
  compatible. libzstd is dual BSD-3-Clause/GPL-2.0 and the BSD option
  applies.

## The engine

Name `gnu-gzip`, format gzip, version `1.14`: the upstream release whose
compressor the port tracks. The version is bumped by hand if a fix to the
port ever changes its output, because the version is what lets `Recompress`
refuse an engine that no longer produces the recorded bytes.

The engine is pure Go and is present in both the cgo and `CGO_ENABLED=0`
builds. `DefaultEngines` lists it first: GNU gzip is the most common
producer of gzip files in the wild, and recording a pure-Go engine keeps the
resulting Params usable from any build.

### Parameters

`DeflateParams` gains `Rsyncable bool` (`json:"rsyncable,omitempty"`),
GNU gzip's `--rsyncable` option. Levels are 1 through 9; GNU gzip has no
level 0. Strategy, window bits and memory level are rejected, as are
`Rsyncable` on the other deflate engines.

Candidates: tier 1 is the nine levels ordered by the XFL hint (`LevelOrder`
without level 0); tier 2 is the same nine levels with `Rsyncable` set.

### The port

Three files mirror the C sources so they can be reviewed side by side:

- `bits.go`: the 16-bit bit buffer, `send_bits`, `bi_reverse`, `bi_windup`,
  `copy_block`. Output goes to a byte slice flushed to the caller's writer.
- `trees.go`: Huffman tree construction, the literal/distance/bit-length
  buffers, `ct_tally` with its flush heuristic, and `flush_block`, including
  the `compressed_len` bookkeeping that decides rsync padding.
- `deflate.go`: the 64 KiB sliding window, 15-bit hash chains, `fill_window`
  with the half-window slide, `longest_match`, the fast (levels 1 to 3) and
  lazy (levels 4 to 9) matchers, and the rsync rolling sum.

Everything that is a file-scope static in C is a field of one `deflater`
struct, so writers are independent and safe to run concurrently in the
search. Static tables (`static_ltree`, `length_code`, `dist_code`, and so
on) are built once at package init.

Two C behaviours need care to be reproduced exactly:

1. **The window is a fixed 2*WSIZE buffer and the code reads a few bytes
   past the valid data** (`INSERT_STRING` reads `window[s+2]`, the matcher
   reads to `strstart+258`). The Go window is two bytes longer than the C
   array so those reads are in bounds; their values never reach the output,
   because matching is disabled once `strstart` exceeds
   `window_size - MIN_LOOKAHEAD` and end-of-input bytes are zeroed exactly
   as C does.
2. **Input reads.** GNU gzip reads the window with `read(2)`, asking for
   `2*WSIZE` at start and then whatever space the slide frees. From a
   regular file each read returns the full amount until end of file. The
   engine models exactly that: the writer buffers incoming bytes and runs
   the compressor only when it can satisfy the pending request in full or
   knows the input has ended. Output therefore does not depend on how the
   caller chunks its writes. GNU gzip reading from a pipe can see short
   reads, which move the point where the window slides; that changes the
   output only when the last few hundred bytes of input land in the
   region where matching is disabled, or when a final match runs past the
   end of input into stale window bytes. Pipe output is not deterministic
   in GNU gzip itself in those cases, so the file behaviour is the
   canonical target.

The compressor is written as a resumable state machine rather than the C
program's single loop: the loop body is one `step`, and `fill_window`
returns "need input" without side effects when the input buffer cannot
satisfy its read. `Write` feeds bytes and advances the machine; `Close`
marks end of input, runs it to completion, and flushes.

## Tests

- `engine/gnugzip`: identity, candidate tiers, parameter rejection, the
  shared `enginetest.RoundTripDeflate` conformance test, and a chunking test
  that feeds the same input in several write sizes and requires identical
  output.
- `TestMatchesGNUGzip` runs the flake's `gzip` binary on temp files across
  the fixture set plus sizes chosen around the window boundaries (64 KiB,
  64 KiB + 32 KiB, and sizes whose end lands in the matching-disabled
  region), at every level and with `--rsyncable`, and compares the deflate
  payload byte for byte. It skips when `gzip` is not GNU gzip.
- `wild_test.go`: the GNU gzip skips are removed and the test no longer
  needs cgo. pigz stays as it was.

## Out of scope

pigz. Its parallel block layout still needs its own engine.
