# Elimination search and confirming pass

Addendum to the zrecipe design of 2026-09-03. Today `Analyze` finds the
engine by recompressing the whole spool with each candidate in tier
order and comparing byte for byte; losers die within their first block,
so the search costs one full recompression of the winner. A caller that
then verifies the pull path (oci-amber's round-trip check) recompresses
the same content a second time. This design splits the work so that a
layer is recompressed once: a cheap elimination isolates the candidate,
and a single confirming pass over the content reproduces the input
through the same code `Recompress` runs, while streaming the content to
the caller for whatever it does with it (oci-amber takes the tar apart).

## Goals

- Recompress the content once when the input is reproducible: the
  elimination touches only a prefix, the confirming pass does the rest.
- The confirming pass runs `Recompress`'s own rebuild code, so a
  parameter set that passes it is one the pull path reproduces by
  construction (the zlib level-0 incident, issue #1, becomes impossible
  by design rather than by discipline).
- Hand the content to the caller during the confirming pass, in order,
  in `engine.FeedSize` blocks, so the caller's processing overlaps the
  recompression.
- Return the same `Params` as today: the earliest candidate in tier
  order that reproduces the input, or `ErrNotReproducible`.
- Never do more work than today's search in the cases the elimination
  cannot settle.

## Non-goals

- Overlapping the elimination with the first pass (it needs only the
  first blocks and could start while pass one still inflates); a later
  refinement.
- Changing the candidate lists, tier order, engines or the `Params`
  schema.

## API

```go
// Start detects the format of r, decompresses it once into a spool while
// hashing both streams, validates the container, and eliminates the
// candidates down to one. ErrUnsupported, ErrCorrupt and the context's
// error are what Analyze returns today; ErrNotReproducible means every
// candidate diverged from the input inside the elimination. The
// *Analysis holds the spool until Close.
func Start(ctx context.Context, r io.ReadSeeker, opts *Options) (*Analysis, error)

type Analysis struct{ /* unexported */ }

func (a *Analysis) Format() Format
func (a *Analysis) Compressed() Digest   // BLAKE3 and size of r
func (a *Analysis) Uncompressed() Digest // BLAKE3 and size of the content
// Verified reports that the elimination already reproduced the whole
// input (a small input, or the fallback search); Confirm still runs the
// rebuild, so the caller need not care.
func (a *Analysis) Verified() bool
// Confirm reads the content once, writes every block to tee (nil to skip)
// and rebuilds the input from it through Recompress's code, comparing the
// output with r byte for byte. It returns the Params on success,
// ErrNotReproducible (with the offset) when the output diverges, tee's
// own error when a tee write fails, the context's error, or an engine
// error. After a failure tee has received a prefix of the content. Confirm
// may be called once.
func (a *Analysis) Confirm(ctx context.Context, tee io.Writer) (*Params, error)
// Close releases the spool. Idempotent.
func (a *Analysis) Close() error
```

`Analyze(ctx, r, opts)` becomes `Start`, `Confirm(ctx, opts.Uncompressed)`,
`Close`, and returns the same `Params` for every input it returns them
for today. The one visible change: `Options.Uncompressed` receives the
content during the confirming pass, not during pass one, so an input
that is not reproducible writes nothing to it, and one whose confirmation
fails writes a prefix. The CLI's `--uncompressed` file already went away
on any failure.

For `FormatNone` `Start` reads r once for the digests and `Confirm`
copies r to tee; there is nothing to eliminate or rebuild, and `Verified`
is true.

## Elimination (`search.Eliminate`)

Every candidate is an engine writer over a comparing writer over the
input's payload, as `evaluate` builds today, plus the position up to
which it has been fed. Candidates advance in lockstep windows over the
spool: the first window ends at 64 KiB, each next one is four times
longer, and the last one ends at the spool's size. Windows are multiples
of `engine.FeedSize` and a candidate is fed from its position to the
window's end in `FeedSize` writes from a section reader over the spool,
so every engine sees exactly the write shape `Recompress` uses. A compare
failure kills the candidate (its writer is closed and dropped). A
candidate that reaches the spool's end is closed, its trailer compared,
and marked complete: a full match.

Within a round the alive candidates are handed out in list order to
`parallelism` workers, one candidate to one worker at a time; a shared
seek-only input (no `io.ReaderAt`) runs one worker and the payload reader
is re-positioned with `Input.Payload(off)` before each slice, so nothing
about the candidate's state depends on the reader. Between rounds a
candidate holds its engine state.

After each round:

- no candidate alive: `ErrNoMatch`, carrying the first non-mismatch error
  as `Run` does;
- a complete candidate: the first complete one in list order is the
  result, `Verified` (every other alive candidate is complete as well,
  and every dead one is a non-match);
- one candidate alive: it is the result once it has matched a margin of
  `FeedSize` bytes past the point where its last competitor died; when
  the window that killed the competitor leaves it short of that, it is
  fed to the margin (rounded up to `FeedSize`, capped at the spool's
  size) first. If that kills it, `ErrNoMatch`;
- otherwise the next round.

The lockstep is capped at two alive candidates. At any moment, in any
round, once more than two candidates have survived the current window
(candidates still being fed do not count), no further candidate is
handed out; the ones in flight finish their window, and the elimination
falls back to `Run`, today's sequential-in-tier-order search, over the
survivors and the candidates the round had not reached yet, in list
order. `Run` restarts each from
the beginning and returns the earliest full match, `Verified`. The cap
bounds two things: the engine states held between rounds, and the work
spent on inputs many candidates agree on for a long stretch (a tar that
opens with a long run of zeros makes every zlib memory level and window
agree until real data appears); on such an input the elimination costs
what the search costs today and nothing more.

Correctness: a candidate that reproduces the input never dies, so it is
in every alive set. The single-survivor exit happens only when every
other candidate is a proven non-match; a fallback evaluates every
candidate that could still match; a complete candidate is a full match.
The result is therefore the earliest full match in list order whenever
one exists, exactly as today, and `Confirm` failing on a single survivor
means no candidate reproduces the input.

`Result` gains `Verified bool`. `Input.Payload` becomes
`func(off int64) (io.Reader, error)`, a reader positioned `off` bytes
into the payload (`io.NewSectionReader` over an `io.ReaderAt`, a `Seek`
on a shared reader otherwise); `Spool` gains `Section(off, n int64)
io.Reader`. `Run` and `evaluate` stay for the fallback and use
`Payload(0)`.

## Confirming pass (`Analysis.Confirm`)

Two goroutines joined by a queued pipe of eight `FeedSize` slots (the
same shape as oci-amber's `blob/pipe.go`; `io.Pipe` would run them in
lockstep):

- the reader reads the spool in `FeedSize` blocks and writes each block
  to tee, then to the pipe; a tee error stops it and is recorded;
- the rebuilder runs `rebuild`, the body of `Recompress` (header, engine
  writer fed through `engine.Feed`, trailer; the zstd frame; the plain
  copy for none) from the pipe into a comparing writer over r from
  offset 0, then checks the comparer is at r's end.

`Recompress` is refactored around the same `rebuild`: it wraps it with
the input and output digest checks it does today. `Confirm` replaces the
output digest check by the byte comparison, which additionally stops the
pass at the first divergent byte instead of at the end.

Failure handling, in priority order once both goroutines have returned:
the context's error; the tee's error, returned wrapped with `%w` and
never turned into anything else; a comparison mismatch, returned as
`ErrNotReproducible` naming the offset; the rebuilder's own error. A
mismatch or a tee error cancels the other goroutine at once (the reader
through a context check per block, the rebuilder through the pipe
closing), so a diverging candidate does not make the pass read the rest
of the spool.

## Budgets

- Elimination CPU: every candidate compresses its first window (64 KiB)
  or less; this is the cost today's search pays for every candidate
  before the winner, now also paid for the candidates after it. On a
  reproducible input the winner's full recompression, today's dominant
  cost, moves to the confirming pass and is paid once.
- Memory: at most two candidates' engine states between rounds plus
  `parallelism` in flight, and the pipe (8 × 32 KiB) during the
  confirming pass. Today's search holds `parallelism` states.
- Disk: unchanged, the spool.

## Testing

`search`:

- Elimination with the fake prefix engine (levels diverge at byte 0):
  small content settles complete and `Verified`; large content settles
  after one window plus the margin and not `Verified`, with the survivor
  fed at most the first window plus the margin (counted by the fake).
- Two candidates agreeing past the first window (a fake whose level sets
  the byte at which it corrupts its output): the loser dies at its byte,
  the winner gets its margin, the result is the winner.
- Three candidates agreeing past the first window: the fallback runs,
  the result is the earliest full match, `Verified`, and no more than two
  engine states existed at once (the fake counts open writers).
- The fallback triggered mid-round covers the candidates the round had
  not reached.
- No match, cancellation, trailer mismatch on a complete candidate, a
  non-mismatch engine error reported through `ErrNoMatch`, and the
  seek-only input, all as `Run`'s tests cover them today.
- Feeding shape: every write a candidate receives is `FeedSize` long
  except the last, across window boundaries.

Root package:

- `Start` then `Confirm` on the gzip and zstd producer cases reproduce
  today's `Params`; tee receives the exact content; `Close` leaves no
  spool file behind.
- `Confirm` with the candidate replaced by a wrong parameter set (a
  white-box test) fails with `ErrNotReproducible` on a multi-MiB input,
  and tee has received less than the whole content.
- A tee that fails makes `Confirm` return that error, unchanged in
  `errors.Is` terms, and the rebuilder stops.
- `Analyze`'s existing tests pass unchanged, `Options.Uncompressed`
  included; the wild-fixture and large-input tests are untouched.
- `Recompress`'s tests pass unchanged after the `rebuild` refactor.

## Rollout

One PR against main, released as v0.5.0. The API additions are `Start`,
`Analysis` and its methods and `search.Result.Verified`; the API changes
inside `search` (`Input.Payload`'s signature) are internal to the module
in practice. The README's library section shows `Start` and `Confirm`
next to `Analyze` and documents the `Options.Uncompressed` timing change.
