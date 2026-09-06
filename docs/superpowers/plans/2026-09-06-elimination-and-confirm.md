# Elimination Search and Confirming Pass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recompress a reproducible input once: `Start` isolates the candidate by lockstep elimination over a prefix of the spool, `Confirm` rebuilds the input through `Recompress`'s code in one pass that also streams the content to the caller.

**Architecture:** `search.Eliminate` advances every candidate in lockstep windows over the spool and falls back to today's `search.Run` when more than two tested candidates stay alive; the root package gains `Start`/`Analysis`/`Confirm` over it, `Recompress` is split into `rebuild` plus digest checks so `Confirm` shares the rebuild, and `Analyze` becomes a wrapper. A queued pipe joins `Confirm`'s reader and rebuilder goroutines.

**Tech Stack:** Go 1.26, cgo engines (zlib, libzstd) via `nix develop`, `lukechampine.com/blake3`.

**Spec:** `docs/superpowers/specs/2026-09-06-elimination-and-confirm-design.md`

## Global Constraints

- Every engine writer is fed through `engine.Feed` (32 KiB writes); windows and margins are multiples of `engine.FeedSize`.
- Same `Params` as today: the earliest candidate in list order that reproduces the input.
- Run the tests inside the flake: `nix develop --command go test ./...` (zlib `TestIdentity` and the wild fixtures fail outside it).
- Commit messages end with the Co-Authored-By and Claude-Session trailers used on this branch.

---

### Task 1: Spool sections and positioned payload readers

**Files:**
- Modify: `search/spool.go` (add `Section`)
- Modify: `search/run.go` (`Input.Payload` signature, `evaluate` uses `Payload(0)`, `Result.Verified`)
- Modify: `analyze.go` (`payloadSource` returns the positioned form)
- Test: `search/spool_test.go`, `search/run_test.go`

**Interfaces:**
- Produces: `func (s *Spool) Section(off, n int64) io.Reader`; `Input.Payload func(off int64) (io.Reader, error)`; `Result.Verified bool` (Run sets true).

- [ ] **Step 1: Write the failing tests**

```go
// spool_test.go
func TestSpoolSection(t *testing.T) {
	for _, maxMem := range []int64{1 << 20, 10} { // in memory, spilled
		sp := NewSpool(t.TempDir(), maxMem)
		sp.Write([]byte("0123456789abcdef"))
		got, _ := io.ReadAll(sp.Section(4, 6))
		if string(got) != "456789" { t.Fatalf("maxMem %d: %q", maxMem, got) }
		got, _ = io.ReadAll(sp.Section(14, 2))
		if string(got) != "ef" { t.Fatalf("maxMem %d: %q", maxMem, got) }
		sp.Close()
	}
}
```

In `run_test.go`, `input()` builds `Payload: func(off int64) (io.Reader, error) { return bytes.NewReader(ref[off:]), nil }` and a new test checks `Run` reports `Verified`:

```go
func TestRunResultIsVerified(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, []byte("content"), 3, true), candidates(e, 1, 3), 1)
	if err != nil || !res.Verified { t.Fatalf("res %+v err %v", res, err) }
}
```

- [ ] **Step 2: Run, expect compile failures** — `nix develop --command go test ./search/`
- [ ] **Step 3: Implement** `Section` (bytes.NewReader over `buf[off:off+n]` or `io.NewSectionReader(file, off, n)`), the `Payload(off)` signature (`analyze.go`'s `payloadSource` adds `off` to the base offset: SectionReader `off+base`, or `Seek(base+off)`), `evaluate` calls `in.Payload(0)`, `Run` returns `Verified: true`.
- [ ] **Step 4: Run the package tests, expect PASS** — `nix develop --command go test ./search/ .`
- [ ] **Step 5: Commit** — `search: spool sections, positioned payload readers, Result.Verified`

### Task 2: `search.Eliminate`

**Files:**
- Create: `search/eliminate.go`
- Test: `search/eliminate_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces: `func Eliminate(ctx context.Context, in *Input, cands []Candidate, parallelism int) (*Result, error)`; constants `FirstWindow = 2 * engine.FeedSize`, `Margin = engine.FeedSize`, `MaxLockstep = 2`, `windowGrowth = 4`.

Algorithm (see the spec): per candidate a `state{idx, w io.WriteCloser, cw *compareWriter, pos int64, dead, complete bool}`. Rounds with `window` = min(size, FirstWindow), then min(size, window*4). A round feeds every alive candidate (list order, `parallelism` workers when `in.Concurrent`, else one) from `pos` to `window`: `cw.ref, err = in.Payload(cw.n)`; `engine.Feed(w, in.Spool.Section(pos, window-pos))`; `ErrMismatch` kills (record the death position `pos+fed`), a context error aborts, another error kills and is remembered as `firstErr`; at `window == size` the writer is closed, the trailer compared, `cw.AtEOF()` checked, and the candidate is complete. A candidate is *tested* once `cw.n > 0`. The hand-out loop stops as soon as more than `MaxLockstep` tested candidates have survived the current window; in-flight ones finish; the round returns `fallback = true`. After a round: none alive → `ErrNoMatch` (wrapping `firstErr` as `Run` does); fallback → `Run(ctx, in, alive ∪ unreached in list order, parallelism)` with indexes mapped back and `Tried` accumulated; a complete candidate → first complete in list order, `Verified: true`; exactly one alive and it is tested → if `window - lastDeath < Margin`, feed it to `min(size, roundUp(lastDeath+Margin, FeedSize))` (complete it if that is the size); alive → result (`Verified` iff complete), dead → `ErrNoMatch`; otherwise next round. Dead candidates' writers are closed and dropped; the result's writer is closed too (its state is not returned).

- [ ] **Step 1: Write the failing tests** (fakes extend `run_test.go`'s: a `divergeAt` fake whose level sets the input offset at which it corrupts one byte, counting bytes written and open writers):

```go
func TestEliminateSettlesSmallInputComplete(t *testing.T)   // 1000 bytes, levels 1,2,3, ref level 2: Index 1, Verified
func TestEliminateSettlesLargeInputAfterMargin(t *testing.T) // 1 MiB, ref level 2: Index 1, !Verified, fake counted <= FirstWindow+Margin bytes for the winner
func TestEliminateTwoAgreeingCandidates(t *testing.T)        // divergeAt: loser corrupts at 300 KiB; winner never: winner wins, loser fed < 1 MiB
func TestEliminateFallsBackAboveTheCap(t *testing.T)         // three candidates agreeing to 200 KiB, one to the end: result is the earliest full match, Verified, and never more than MaxLockstep+parallelism writers open
func TestEliminateFallbackCoversUnreachedCandidates(t *testing.T) // cap hit mid-round with the true producer last in the list: still found
func TestEliminateNoMatch(t *testing.T)
func TestEliminateCancelled(t *testing.T)
func TestEliminateTrailerMismatch(t *testing.T)              // small input, wrong trailer: ErrNoMatch
func TestEliminateReportsNonMismatchError(t *testing.T)      // errDeflate: ErrNoMatch message carries it
func TestEliminateSequentialInput(t *testing.T)              // Concurrent=false, parallelism 4, several rounds: same result
func TestEliminateFeedsFixedSizeWrites(t *testing.T)         // every write FeedSize except the last, across window boundaries
```

- [ ] **Step 2: Run, expect FAIL (undefined: Eliminate)**
- [ ] **Step 3: Implement `search/eliminate.go`**
- [ ] **Step 4: `nix develop --command go test -race ./search/`, expect PASS**
- [ ] **Step 5: Commit** — `search: lockstep elimination with fallback to the sequential search`

### Task 3: `rebuild` split out of `Recompress`

**Files:**
- Modify: `recompress.go`
- Test: `recompress_test.go` (existing tests must pass unchanged)

**Interfaces:**
- Produces: `func rebuild(ctx context.Context, p *Params, in io.Reader, out io.Writer, o *RecompressOptions) error` — the format switch (`io.Copy` for none, `recompressGzip`, `recompressZstd`) over `out` wrapped in `ctxWriter`; `Recompress` = version and validation checks, then `rebuild` with the hashing wrappers and the digest checks exactly as today.

- [ ] **Step 1: Refactor; no behaviour change**
- [ ] **Step 2: `nix develop --command go test .` PASS**
- [ ] **Step 3: Commit** — `recompress: split rebuild out of Recompress`

### Task 4: Queued pipe

**Files:**
- Create: `pipe.go` (root package, unexported)
- Test: `pipe_test.go`

**Interfaces:**
- Produces: `newPipe(slots int) *pipe` with `Write([]byte) (int, error)` (copies, blocks while full, `io.ErrClosedPipe` after `CloseRead`), `CloseWrite(err error)` (readers see the queued data then `err` or `io.EOF`), `Read([]byte) (int, error)`, `CloseRead()` (idempotent; unblocks and fails the writer). The same contract as oci-amber's `blob/pipe.go`.

- [ ] **Step 1: Tests** — order and EOF, copies bytes, CloseWrite error follows the data, CloseRead unblocks a blocked writer and fails later writes, CloseRead idempotent.
- [ ] **Step 2: Implement; `go test -race -run Pipe .` PASS**
- [ ] **Step 3: Commit** — `zrecipe: queued pipe for the confirming pass`

### Task 5: `Start`, `Analysis`, `Confirm`, `Analyze` as a wrapper

**Files:**
- Create: `analysis.go` (`Analysis`, `Start`, `Confirm`, `Close`)
- Modify: `analyze.go` (`analyzeGzip`/`analyzeZstd` return the pieces `Start` keeps instead of `*Params`; `Analyze` wraps; `Options.Uncompressed` doc)
- Test: `analysis_test.go`, existing `analyze_test.go`

**Interfaces:**
- Consumes: `search.Eliminate`, `rebuild`, `newPipe`.
- Produces: the API in the spec. Internals: `Analysis{format, opts, r io.ReadSeeker, spool *search.Spool, compressed, uncompressed Digest, params *Params (complete, engine filled from the elimination's candidate), verified bool, confirmed bool, closed bool}`.

`Start`: `format.Detect`; none → read r once through `newHasher` (no spool), `verified = true`; gzip/zstd → today's pass one into the spool, then `search.Eliminate` in place of `runSearch` (map `ErrNoMatch` to `ErrNotReproducible`); build `Params` for the result candidate. On any error the spool is closed before returning.

`Confirm(ctx, tee)`: none → `Seek(0)`, copy r to tee (nil tee → discard) through `ctxWriter`; return params. Otherwise: `cctx, cancel := context.WithCancelCause(ctx)`; `p := newPipe(8)`; reader goroutine: `buf := make([]byte, engine.FeedSize)`; loop `io.ReadFull(spool.Reader())`: write to tee (error → `teeErr = err; cancel(err)`; stop), then `p.Write`; at end `p.CloseWrite(nil)` (or the error). Rebuilder goroutine: `ref, _ := a.payload(0)` over the whole input from offset 0 (r via ReaderAt section or Seek); `cw := newCompareWriter(cctx, ref)`; `err := rebuild(cctx, a.params, p, cw, &RecompressOptions{Engines: a.opts.Engines})`; on success `err = cw.AtEOF()`; on any error `p.CloseRead(); cancel(err)`. Wait for both. Result priority: `ctx.Err()`; `teeErr` (returned as `fmt.Errorf("zrecipe: uncompressed writer: %w", teeErr)`); `errors.Is(err, search.ErrMismatch)` → `fmt.Errorf("%w: %v", ErrNotReproducible, err)`; else the rebuilder's error. `confirmed = true` on success. Confirm twice → error.

`Analyze`: `a, err := Start(...)`; `defer a.Close()`; `return a.Confirm(ctx, o.Uncompressed)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestStartConfirmGzipProducers(t *testing.T)  // reuse gzipProducerCases: Start, Confirm with a bytes.Buffer tee; Params equal analyze()'s; tee == data; a.Verified() is false for the >FirstWindow+Margin cases
func TestStartConfirmZstdProducers(t *testing.T)
func TestStartNone(t *testing.T)                   // Format none, Confirm copies r to tee, equal digests
func TestStartNotReproducibleWritesNothing(t *testing.T) // zstd-only engines on a gzip: Start fails, Uncompressed untouched
func TestConfirmDivergingCandidate(t *testing.T)  // 4 MiB text gzip at goflate 6: Start; a.params.Gzip.Level = 9; Confirm → ErrNotReproducible, tee shorter than data
func TestConfirmTeeErrorIsReturned(t *testing.T)  // tee fails after 100 KiB with errBoom: errors.Is(err, errBoom), Confirm returns promptly
func TestConfirmCancelled(t *testing.T)
func TestCloseRemovesSpool(t *testing.T)          // MaxInMemory 1: no zrecipe-spool-* under TempDir after Close
func TestConfirmTwiceFails(t *testing.T)
```

- [ ] **Step 2: Run, expect FAIL**
- [ ] **Step 3: Implement**
- [ ] **Step 4: `nix develop --command go test -race ./...` PASS (analyze_test, recompress_test, wild_test included)**
- [ ] **Step 5: Commit** — `zrecipe: Start, Analysis.Confirm; Analyze as a wrapper`

### Task 6: pigz streams its first job

**Files:**
- Modify: `engine/pigz/pigz.go` (`parallel`: job 0's compressor writes to `out` directly; the writer goroutine does not re-write it)
- Test: `engine/pigz/parallel_test.go` (`TestMismatchCostsOneJob` gets a sibling: a destination failing at its first write makes `Write`/`Close` fail before the first job has been fully compressed — measure with a large block and a level-9 job: the failure arrives with far less CPU than compressing the whole job)

- [ ] **Step 1: Test; Step 2: implement; Step 3: `nix develop --command go test -race ./engine/pigz/` PASS (equivalence tests, TestMatchesPigz)**
- [ ] **Step 4: Commit** — `pigz: stream the first job's output as it is produced`

### Task 7: Docs, README, release

**Files:**
- Modify: `README.md` (library usage: `Start`/`Confirm` example after `Analyze`; the `Options.Uncompressed` timing; the write-size independence paragraph now says the elimination and the confirming pass both feed through `engine.Feed`; Testing unchanged)

- [ ] **Step 1: Edit README; Step 2: `nix develop --command go test ./...`; Step 3: Commit** — `README: Start and Confirm`
- [ ] **Step 4: PR against main, merge, `gh release create v0.5.0 --target main` with notes in the v0.4.0 shape (intro, "Elimination search and confirming pass (#N)", Tests, Compatibility, Install).**
