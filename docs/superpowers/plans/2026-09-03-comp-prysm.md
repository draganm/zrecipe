# comp-prysm Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A Go library plus CLI that detects gzip/zstd input, finds engine parameters that reproduce the compressed bytes exactly, records them with blake3 digests as JSON, and rebuilds the compressed file from uncompressed content with verification.

**Architecture:** One streaming pass decompresses, hashes and spools the content. An ordered candidate list built from five engines (cgo zlib, cgo libzstd, stdlib flate, klauspost flate, klauspost zstd) is evaluated by re-compressing the spool into a compare writer that aborts at the first differing byte. Engines only produce payloads: the library owns the gzip header and trailer.

**Tech Stack:** Go 1.26 (nixpkgs 26.05), cgo against zlib 1.3.2 and libzstd 1.5.7, `github.com/klauspost/compress` v1.20.0, `lukechampine.com/blake3` v1.4.1, `github.com/urfave/cli/v2` v2.27.7.

**Spec:** `docs/superpowers/specs/2026-09-03-comp-prysm-design.md`

## Global Constraints

- Module path `github.com/draganm/comp-prysm`, root package `compprysm`. No `internal` packages.
- All commands run inside the flake dev shell: prefix with `nix develop --command` (or rely on direnv). Expect and ignore `warning: Git tree ... is dirty`.
- The `Format` type lives in package `format` (the lowest package) and is re-exported by alias from `engine` and the root, since `engine` imports `format` for header types. This is the one deliberate deviation from the spec text.
- `ZstdParams` gains one field beyond the spec, `EncodeAll bool json:"encode_all,omitempty"`, used only by klauspost-zstd to mark frames produced by a one-shot `EncodeAll` call.
- Engines never write the gzip header or trailer. Engine writers must propagate the underlying writer's error from `Write` and `Close` and stop compressing after it.
- Every commit message ends with:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016GtY66cGSjYnDNVqbdf1tw
  ```
- Remove any binaries you build (`go build -o ...`) before finishing a task.
- Tests must pass with `go test ./...` and `go vet ./...` must be clean before each commit.

## File structure

| File | Responsibility |
|---|---|
| `flake.nix` | dev shell with go, zlib, zstd, pkg-config, gzip, pigz |
| `format/format.go` | `Format` type, magic detection |
| `format/gzip.go` | gzip header parsing, raw bytes captured |
| `format/zstd.go` | zstd frame header parsing, frame length walk |
| `engine/engine.go` | `Engine`, `DeflateEngine`, `ZstdEngine`, param types, helpers |
| `search/spool.go` | memory-then-file spool with concurrent readers |
| `search/compare.go` | compare writer with early abort |
| `search/run.go` | candidate evaluation, deterministic parallel search |
| `fixtures/fixtures.go` | deterministic sample inputs for tests |
| `enginetest/enginetest.go` | shared round-trip conformance tests for engines |
| `engine/goflate/goflate.go` | stdlib flate engine |
| `engine/kpflate/kpflate.go` | klauspost flate engine |
| `engine/zlib/zlib.go` | cgo zlib raw deflate engine |
| `engine/libzstd/libzstd.go` | cgo libzstd engine |
| `engine/kpzstd/kpzstd.go` | klauspost zstd engine |
| `format.go`, `errors.go`, `params.go`, `engines.go`, `engines_cgo.go`, `engines_nocgo.go`, `io.go` | root package types and helpers |
| `analyze.go` | `Analyze`: pass 1 and search per format |
| `recompress.go` | `Recompress` with digest verification |
| `wild_test.go` | fixtures produced by real gzip, pigz, zstd CLIs |
| `large_test.go` | 2 GiB gated test |
| `cmd/comp-prysm/main.go` | urfave/cli CLI |
| `README.md` | usage and limitations |

---

### Task 1: Dev shell with C libraries and tools

**Files:**
- Modify: `flake.nix`

**Interfaces:**
- Produces: a dev shell where `pkg-config --modversion zlib libzstd` prints `1.3.2` and `1.5.7`, and `gzip`, `pigz`, `zstd` are on PATH.

- [ ] **Step 1: Add packages to the dev shell**

Replace the `packages` line in `flake.nix` with:

```nix
          packages = with pkgs; [ go pkg-config zlib zlib.dev zstd zstd.dev gzip pigz ];
```

- [ ] **Step 2: Verify the shell**

Run:
```bash
nix develop --command bash -c 'pkg-config --modversion zlib libzstd; which gzip pigz zstd; go version'
```
Expected: `1.3.2`, `1.5.7`, three `/nix/store/...` paths, `go version go1.26.6 ...`. If pkg-config reports a different zlib version, `PKG_CONFIG_PATH` is picking up an ambient zlib; run `nix develop --command bash -c 'echo $PKG_CONFIG_PATH'` and confirm the nix store zlib `dev` path is first. Do not proceed until both versions are right.

- [ ] **Step 3: Commit**

```bash
git add flake.nix flake.lock
git commit -m "Add C libraries and reference tools to dev shell"
```

---

### Task 2: format package: detection and gzip header

**Files:**
- Create: `format/format.go`, `format/gzip.go`, `format/format_test.go`, `format/gzip_test.go`

**Interfaces:**
- Produces:
  - `type Format string`; `const None, Gzip, Zstd Format`
  - `func Detect(r io.ReadSeeker) (Format, error)` seeks back to 0.
  - `func DetectBytes(b []byte) Format`
  - `type GzipHeader struct { Raw []byte; Flags byte; ModTime uint32; XFL byte; OS byte }`
  - `func ParseGzipHeader(r *bufio.Reader) (*GzipHeader, error)` leaves `r` at the first deflate byte.
  - `var ErrBadHeader = errors.New("format: bad header")`

- [ ] **Step 1: Write failing tests**

`format/format_test.go`:
```go
package format

import (
	"bytes"
	"testing"
)

func TestDetectBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want Format
	}{
		{"gzip", []byte{0x1f, 0x8b, 8, 0}, Gzip},
		{"zstd", []byte{0x28, 0xb5, 0x2f, 0xfd, 0}, Zstd},
		{"zstd skippable", []byte{0x5a, 0x2a, 0x4d, 0x18}, Zstd},
		{"plain", []byte("hello"), None},
		{"empty", nil, None},
		{"one byte", []byte{0x1f}, None},
		{"zstd prefix only", []byte{0x28, 0xb5, 0x2f}, None},
	}
	for _, c := range cases {
		if got := DetectBytes(c.in); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestDetectSeeksBack(t *testing.T) {
	r := bytes.NewReader([]byte{0x1f, 0x8b, 8, 0, 1, 2, 3})
	r.Seek(3, 0)
	f, err := Detect(r)
	if err != nil {
		t.Fatal(err)
	}
	if f != Gzip {
		t.Fatalf("got %q", f)
	}
	if pos, _ := r.Seek(0, 1); pos != 0 {
		t.Fatalf("reader at %d, want 0", pos)
	}
}

func TestDetectShortInput(t *testing.T) {
	f, err := Detect(bytes.NewReader([]byte{1}))
	if err != nil || f != None {
		t.Fatalf("got %q, %v", f, err)
	}
	f, err = Detect(bytes.NewReader(nil))
	if err != nil || f != None {
		t.Fatalf("empty: got %q, %v", f, err)
	}
}
```

`format/gzip_test.go`:
```go
package format

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"hash/crc32"
	"io"
	"testing"
	"time"
)

func gzipWith(t *testing.T, hdr gzip.Header, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Header = hdr
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseGzipHeaderMinimal(t *testing.T) {
	data := []byte("hello gzip")
	file := gzipWith(t, gzip.Header{}, data)
	br := bufio.NewReader(bytes.NewReader(file))
	h, err := ParseGzipHeader(br)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Raw) != 10 {
		t.Fatalf("raw header length %d, want 10", len(h.Raw))
	}
	if !bytes.Equal(h.Raw, file[:10]) {
		t.Fatalf("raw header differs from file prefix")
	}
	// The rest must be a deflate stream followed by an 8 byte trailer.
	rest, _ := io.ReadAll(flate.NewReader(br))
	if !bytes.Equal(rest, data) {
		t.Fatalf("deflate payload does not decode to data")
	}
	trailer := make([]byte, 8)
	if _, err := io.ReadFull(br, trailer); err != nil {
		t.Fatal(err)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("expected EOF after trailer, got %v", err)
	}
}

func TestParseGzipHeaderAllFields(t *testing.T) {
	hdr := gzip.Header{
		Name:    "file.txt",
		Comment: "a comment",
		Extra:   []byte{1, 2, 3, 4, 5},
		ModTime: time.Unix(1700000000, 0),
		OS:      3,
	}
	file := gzipWith(t, hdr, []byte("payload"))
	h, err := ParseGzipHeader(bufio.NewReader(bytes.NewReader(file)))
	if err != nil {
		t.Fatal(err)
	}
	want := 10 + 2 + len(hdr.Extra) + len(hdr.Name) + 1 + len(hdr.Comment) + 1
	if len(h.Raw) != want {
		t.Fatalf("raw length %d, want %d", len(h.Raw), want)
	}
	if !bytes.Equal(h.Raw, file[:want]) {
		t.Fatalf("raw header differs from file prefix")
	}
	if h.ModTime != 1700000000 || h.OS != 3 || h.Flags&0x1c != 0x1c {
		t.Fatalf("fields: mtime %d os %d flags %#x", h.ModTime, h.OS, h.Flags)
	}
}

func TestParseGzipHeaderHCRC(t *testing.T) {
	// Hand-built header with FHCRC set: fixed 10 bytes, then CRC16 of them.
	fixed := []byte{0x1f, 0x8b, 8, 0x02, 0, 0, 0, 0, 0, 0xff}
	crc := crc32.ChecksumIEEE(fixed)
	file := append([]byte{}, fixed...)
	file = append(file, byte(crc), byte(crc>>8))
	file = append(file, 0x03, 0x00) // empty final deflate block
	file = append(file, 0, 0, 0, 0, 0, 0, 0, 0)
	h, err := ParseGzipHeader(bufio.NewReader(bytes.NewReader(file)))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Raw) != 12 || !bytes.Equal(h.Raw, file[:12]) {
		t.Fatalf("raw %x", h.Raw)
	}
}

func TestParseGzipHeaderErrors(t *testing.T) {
	cases := map[string][]byte{
		"truncated fixed": {0x1f, 0x8b, 8},
		"bad magic":       {0x1f, 0x8c, 8, 0, 0, 0, 0, 0, 0, 0},
		"bad method":      {0x1f, 0x8b, 7, 0, 0, 0, 0, 0, 0, 0},
		"truncated name":  {0x1f, 0x8b, 8, 0x08, 0, 0, 0, 0, 0, 0, 'a', 'b'},
		"truncated extra": {0x1f, 0x8b, 8, 0x04, 0, 0, 0, 0, 0, 0, 5, 0, 1},
	}
	for name, in := range cases {
		_, err := ParseGzipHeader(bufio.NewReader(bytes.NewReader(in)))
		if !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s: got %v, want ErrBadHeader", name, err)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./format/`
Expected: build failure, `undefined: DetectBytes` etc.

- [ ] **Step 3: Implement**

`format/format.go`:
```go
// Package format detects compression formats and parses their headers.
package format

import (
	"errors"
	"io"
)

// Format identifies a compression container.
type Format string

const (
	None Format = "none"
	Gzip Format = "gzip"
	Zstd Format = "zstd"
)

// ErrBadHeader reports a header that cannot be parsed.
var ErrBadHeader = errors.New("format: bad header")

// Detect reads the magic bytes of r and seeks it back to the start.
// An input shorter than any magic sequence is None.
func Detect(r io.ReadSeeker) (Format, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return None, err
	}
	var buf [4]byte
	n, err := io.ReadFull(r, buf[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return None, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return None, err
	}
	return DetectBytes(buf[:n]), nil
}

// DetectBytes identifies the format from the leading bytes of a file. A
// zstd skippable frame magic (0x184D2A50..5F) also reports Zstd, so that
// the frame parser can reject it explicitly.
func DetectBytes(b []byte) Format {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		return Gzip
	}
	if len(b) >= 4 && b[0] == 0x28 && b[1] == 0xb5 && b[2] == 0x2f && b[3] == 0xfd {
		return Zstd
	}
	if len(b) >= 4 && b[0]&0xf0 == 0x50 && b[1] == 0x2a && b[2] == 0x4d && b[3] == 0x18 {
		return Zstd
	}
	return None
}
```

`format/gzip.go`:
```go
package format

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// GzipHeader is a parsed gzip member header. Raw holds the exact header
// bytes, from byte 0 up to the first byte of the deflate stream.
type GzipHeader struct {
	Raw     []byte
	Flags   byte
	ModTime uint32
	XFL     byte
	OS      byte
}

const (
	gzipFHCRC    = 0x02
	gzipFEXTRA   = 0x04
	gzipFNAME    = 0x08
	gzipFCOMMENT = 0x10
)

// ParseGzipHeader reads a gzip member header from r and leaves r positioned
// at the first byte of the deflate stream.
func ParseGzipHeader(r *bufio.Reader) (*GzipHeader, error) {
	fixed := make([]byte, 10)
	if _, err := io.ReadFull(r, fixed); err != nil {
		return nil, fmt.Errorf("%w: gzip fixed header: %v", ErrBadHeader, err)
	}
	if fixed[0] != 0x1f || fixed[1] != 0x8b {
		return nil, fmt.Errorf("%w: not a gzip magic", ErrBadHeader)
	}
	if fixed[2] != 8 {
		return nil, fmt.Errorf("%w: gzip compression method %d", ErrBadHeader, fixed[2])
	}
	h := &GzipHeader{
		Raw:     append([]byte{}, fixed...),
		Flags:   fixed[3],
		ModTime: binary.LittleEndian.Uint32(fixed[4:8]),
		XFL:     fixed[8],
		OS:      fixed[9],
	}
	if h.Flags&gzipFEXTRA != 0 {
		var xlen [2]byte
		if _, err := io.ReadFull(r, xlen[:]); err != nil {
			return nil, fmt.Errorf("%w: gzip extra length: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, xlen[:]...)
		extra := make([]byte, binary.LittleEndian.Uint16(xlen[:]))
		if _, err := io.ReadFull(r, extra); err != nil {
			return nil, fmt.Errorf("%w: gzip extra field: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, extra...)
	}
	for _, f := range []struct {
		flag byte
		name string
	}{{gzipFNAME, "name"}, {gzipFCOMMENT, "comment"}} {
		if h.Flags&f.flag == 0 {
			continue
		}
		s, err := r.ReadBytes(0)
		if err != nil {
			return nil, fmt.Errorf("%w: gzip %s: %v", ErrBadHeader, f.name, err)
		}
		h.Raw = append(h.Raw, s...)
	}
	if h.Flags&gzipFHCRC != 0 {
		var crc [2]byte
		if _, err := io.ReadFull(r, crc[:]); err != nil {
			return nil, fmt.Errorf("%w: gzip header crc: %v", ErrBadHeader, err)
		}
		h.Raw = append(h.Raw, crc[:]...)
	}
	return h, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./format/ -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add format
git commit -m "Add format detection and gzip header parsing"
```

---

### Task 3: format package: zstd frame header and frame length

**Files:**
- Create: `format/zstd.go`, `format/zstd_test.go`
- Modify: `go.mod` (adds klauspost/compress for tests)

**Interfaces:**
- Produces:
  - `type ZstdFrameHeader struct { HeaderLen int; WindowLog int; WindowSize uint64; SingleSegment bool; Checksum bool; HasContentSize bool; ContentSize uint64; DictID uint32 }`
  - `func ParseZstdFrameHeader(r *bufio.Reader) (*ZstdFrameHeader, error)`
  - `func ZstdFrameLength(r *bufio.Reader) (*ZstdFrameHeader, int64, error)` walks block headers without decoding; returns the total frame length including checksum.
  - `var ErrSkippableFrame = errors.New("format: skippable frame")`

- [ ] **Step 1: Add the klauspost dependency**

Run: `nix develop --command go get github.com/klauspost/compress@v1.20.0`

- [ ] **Step 2: Write failing tests**

`format/zstd_test.go`:
```go
package format

import (
	"bufio"
	"bytes"
	"errors"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdAll(t *testing.T, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil, append(opts, zstd.WithEncoderConcurrency(1), zstd.WithZeroFrames(true))...)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

func zstdStream(t *testing.T, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf, append(opts, zstd.WithEncoderConcurrency(1))...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sample(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/13)
	}
	return b
}

func parse(t *testing.T, frame []byte) *ZstdFrameHeader {
	t.Helper()
	h, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestZstdHeaderSingleSegmentWithChecksum(t *testing.T) {
	data := sample(1000)
	h := parse(t, zstdAll(t, data, zstd.WithEncoderCRC(true), zstd.WithSingleSegment(true)))
	if !h.SingleSegment || !h.Checksum || !h.HasContentSize || h.ContentSize != 1000 {
		t.Fatalf("%+v", h)
	}
	if h.WindowLog != 0 || h.WindowSize != 1000 {
		t.Fatalf("window: %+v", h)
	}
}

func TestZstdHeaderStreamed(t *testing.T) {
	h := parse(t, zstdStream(t, sample(300000), zstd.WithEncoderCRC(false), zstd.WithWindowSize(1<<16)))
	if h.SingleSegment || h.Checksum || h.HasContentSize {
		t.Fatalf("%+v", h)
	}
	if h.WindowLog != 16 || h.WindowSize != 1<<16 {
		t.Fatalf("window: %+v", h)
	}
}

func TestZstdHeaderDictID(t *testing.T) {
	// Magic, descriptor with dict id flag 2 (2 bytes), window descriptor, dict id 0x1234.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x02, 0x40, 0x34, 0x12}
	h := parse(t, frame)
	if h.DictID != 0x1234 || h.HeaderLen != 8 {
		t.Fatalf("%+v", h)
	}
}

func TestZstdHeaderSkippable(t *testing.T) {
	frame := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	_, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if !errors.Is(err, ErrSkippableFrame) {
		t.Fatalf("got %v", err)
	}
}

func TestZstdHeaderErrors(t *testing.T) {
	for name, in := range map[string][]byte{
		"truncated": {0x28, 0xb5},
		"bad magic": {0x28, 0xb5, 0x2f, 0xfe, 0},
		"reserved":  {0x28, 0xb5, 0x2f, 0xfd, 0x08, 0x40},
	} {
		_, err := ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(in)))
		if !errors.Is(err, ErrBadHeader) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestZstdFrameLength(t *testing.T) {
	for name, frame := range map[string][]byte{
		"all crc":      zstdAll(t, sample(500000), zstd.WithEncoderCRC(true)),
		"all nocrc":    zstdAll(t, sample(500000), zstd.WithEncoderCRC(false)),
		"stream crc":   zstdStream(t, sample(500000), zstd.WithEncoderCRC(true)),
		"empty":        zstdAll(t, nil, zstd.WithEncoderCRC(true)),
		"zeros":        zstdStream(t, make([]byte, 1<<20)),
		"single block": zstdAll(t, []byte("tiny"), zstd.WithEncoderCRC(false)),
	} {
		_, n, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(frame)))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if n != int64(len(frame)) {
			t.Errorf("%s: length %d, want %d", name, n, len(frame))
		}
	}
}

func TestZstdFrameLengthStopsAtFirstFrame(t *testing.T) {
	a := zstdAll(t, sample(1000), zstd.WithEncoderCRC(true))
	b := zstdAll(t, sample(2000), zstd.WithEncoderCRC(true))
	_, n, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(append(append([]byte{}, a...), b...))))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(a)) {
		t.Fatalf("length %d, want %d", n, len(a))
	}
}

func TestZstdFrameLengthTruncated(t *testing.T) {
	a := zstdAll(t, sample(100000), zstd.WithEncoderCRC(true))
	_, _, err := ZstdFrameLength(bufio.NewReader(bytes.NewReader(a[:len(a)-10])))
	if !errors.Is(err, ErrBadHeader) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `nix develop --command go test ./format/`
Expected: build failure, `undefined: ParseZstdFrameHeader`.

- [ ] **Step 4: Implement**

`format/zstd.go`:
```go
package format

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ErrSkippableFrame reports a zstd skippable frame at the start of the input.
var ErrSkippableFrame = errors.New("format: skippable frame")

// ZstdFrameHeader is a parsed zstd frame header (RFC 8878 section 3.1.1).
type ZstdFrameHeader struct {
	HeaderLen      int    // bytes from the magic through the last header field
	WindowLog      int    // 0 when the frame has no window descriptor
	WindowSize     uint64 // from the descriptor, or the content size for single segment frames
	SingleSegment  bool
	Checksum       bool
	HasContentSize bool
	ContentSize    uint64
	DictID         uint32
}

// ParseZstdFrameHeader reads a zstd frame header from r and leaves r at the
// first block header.
func ParseZstdFrameHeader(r *bufio.Reader) (*ZstdFrameHeader, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return nil, fmt.Errorf("%w: zstd magic: %v", ErrBadHeader, err)
	}
	if magic[0]&0xf0 == 0x50 && magic[1] == 0x2a && magic[2] == 0x4d && magic[3] == 0x18 {
		return nil, ErrSkippableFrame
	}
	if magic != [4]byte{0x28, 0xb5, 0x2f, 0xfd} {
		return nil, fmt.Errorf("%w: not a zstd magic", ErrBadHeader)
	}
	fhd, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("%w: zstd frame header descriptor: %v", ErrBadHeader, err)
	}
	if fhd&0x08 != 0 {
		return nil, fmt.Errorf("%w: zstd reserved descriptor bit set", ErrBadHeader)
	}
	h := &ZstdFrameHeader{
		HeaderLen:     5,
		SingleSegment: fhd&0x20 != 0,
		Checksum:      fhd&0x04 != 0,
	}
	if !h.SingleSegment {
		wd, err := r.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("%w: zstd window descriptor: %v", ErrBadHeader, err)
		}
		h.HeaderLen++
		h.WindowLog = 10 + int(wd>>3)
		base := uint64(1) << h.WindowLog
		h.WindowSize = base + (base/8)*uint64(wd&7)
	}
	dictSize := [4]int{0, 1, 2, 4}[fhd&0x03]
	if dictSize > 0 {
		var b [4]byte
		if _, err := io.ReadFull(r, b[:dictSize]); err != nil {
			return nil, fmt.Errorf("%w: zstd dictionary id: %v", ErrBadHeader, err)
		}
		h.HeaderLen += dictSize
		h.DictID = binary.LittleEndian.Uint32(b[:])
	}
	fcsSize := 0
	switch fhd >> 6 {
	case 0:
		if h.SingleSegment {
			fcsSize = 1
		}
	case 1:
		fcsSize = 2
	case 2:
		fcsSize = 4
	case 3:
		fcsSize = 8
	}
	if fcsSize > 0 {
		var b [8]byte
		if _, err := io.ReadFull(r, b[:fcsSize]); err != nil {
			return nil, fmt.Errorf("%w: zstd content size: %v", ErrBadHeader, err)
		}
		h.HeaderLen += fcsSize
		h.HasContentSize = true
		h.ContentSize = binary.LittleEndian.Uint64(b[:])
		if fcsSize == 2 {
			h.ContentSize += 256
		}
	}
	if h.SingleSegment {
		h.WindowSize = h.ContentSize
	}
	return h, nil
}

// ZstdFrameLength parses the frame header and walks the block headers,
// without decompressing, to find the total length of the first frame in r
// including its checksum. r is consumed up to the end of the frame.
func ZstdFrameLength(r *bufio.Reader) (*ZstdFrameHeader, int64, error) {
	h, err := ParseZstdFrameHeader(r)
	if err != nil {
		return nil, 0, err
	}
	n := int64(h.HeaderLen)
	for {
		var bh [3]byte
		if _, err := io.ReadFull(r, bh[:]); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd block header: %v", ErrBadHeader, err)
		}
		v := uint32(bh[0]) | uint32(bh[1])<<8 | uint32(bh[2])<<16
		last := v&1 != 0
		size := int(v >> 3)
		switch (v >> 1) & 3 {
		case 1: // RLE block: one byte on disk
			size = 1
		case 3:
			return nil, 0, fmt.Errorf("%w: zstd reserved block type", ErrBadHeader)
		}
		if _, err := r.Discard(size); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd block truncated: %v", ErrBadHeader, err)
		}
		n += 3 + int64(size)
		if last {
			break
		}
	}
	if h.Checksum {
		if _, err := r.Discard(4); err != nil {
			return nil, 0, fmt.Errorf("%w: zstd checksum truncated: %v", ErrBadHeader, err)
		}
		n += 4
	}
	return h, n, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix develop --command go test ./format/ -v`
Expected: all PASS. If `TestZstdHeaderSingleSegmentWithChecksum` fails because klauspost did not set single segment, check the frame's descriptor byte: `WithSingleSegment(true)` must be honored by `EncodeAll`; if the flag is missing, read the klauspost source for the condition and adjust the test input size, not the parser.

- [ ] **Step 6: Commit**

```bash
git add format go.mod go.sum
git commit -m "Add zstd frame header parsing and frame length walk"
```

---

### Task 4: engine package: interfaces and parameter types

**Files:**
- Create: `engine/engine.go`, `engine/engine_test.go`

**Interfaces:**
- Consumes: `format.Format`, `format.GzipHeader`, `format.ZstdFrameHeader`
- Produces:
  - `type Format = format.Format`; `const FormatNone, FormatGzip, FormatZstd`
  - `type Engine interface { Name() string; Version() string; Format() Format }`
  - `type DeflateParams struct { Level int; Strategy string; WindowBits int; MemLevel int }` with json tags `level`, `strategy,omitempty`, `window_bits,omitempty`, `mem_level,omitempty`
  - `type GzipParams struct { HeaderB64 string json:"header_b64"; DeflateParams }`
  - `type ZstdParams struct { Level int; WindowLog int; Checksum bool; ContentSize bool; PledgedSize bool; SingleSegment bool; Workers int; Long bool; EncodeAll bool }` json tags `level`, `window_log,omitempty`, `checksum`, `content_size`, `pledged_size`, `single_segment`, `workers`, `long,omitempty`, `encode_all,omitempty`
  - `type DeflateEngine interface { Engine; Candidates(h *format.GzipHeader, uncompressedSize int64) [][]DeflateParams; NewWriter(w io.Writer, p DeflateParams) (io.WriteCloser, error) }`
  - `type ZstdEngine interface { Engine; Candidates(h *format.ZstdFrameHeader, uncompressedSize int64) [][]ZstdParams; NewWriter(w io.Writer, p ZstdParams, uncompressedSize int64) (io.WriteCloser, error) }`
  - `const StrategyDefault = "default"; StrategyFiltered = "filtered"; StrategyHuffmanOnly = "huffman_only"; StrategyRLE = "rle"; StrategyFixed = "fixed"`
  - `func LevelOrder(xfl byte) []int`
  - `func ModuleVersion(path string) string`
  - `func ByName(engines []Engine, name string) (Engine, bool)`

- [ ] **Step 1: Write failing tests**

`engine/engine_test.go`:
```go
package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestLevelOrder(t *testing.T) {
	cases := map[byte][]int{
		2: {9, 8, 7, 6, 5, 4, 3, 2, 1, 0},
		4: {1, 2, 3, 4, 5, 6, 7, 8, 9, 0},
		0: {6, 5, 7, 4, 8, 3, 9, 2, 1, 0},
		7: {6, 5, 7, 4, 8, 3, 9, 2, 1, 0},
	}
	for xfl, want := range cases {
		if got := LevelOrder(xfl); !reflect.DeepEqual(got, want) {
			t.Errorf("xfl %d: got %v want %v", xfl, got, want)
		}
	}
}

func TestGzipParamsJSONIsFlat(t *testing.T) {
	p := GzipParams{HeaderB64: "H4sI", DeflateParams: DeflateParams{Level: 6, Strategy: StrategyDefault, WindowBits: 15, MemLevel: 8}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"header_b64":"H4sI","level":6,"strategy":"default","window_bits":15,"mem_level":8}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
	var back GzipParams
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back != p {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestZstdParamsJSONOmitsOptional(t *testing.T) {
	b, err := json.Marshal(ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"level":3,"checksum":true,"content_size":true,"pledged_size":true,"single_segment":false,"workers":0}`
	if string(b) != want {
		t.Fatalf("got %s", b)
	}
}

func TestModuleVersionUnknown(t *testing.T) {
	if v := ModuleVersion("example.com/does/not/exist"); v != "(devel)" {
		t.Fatalf("got %q", v)
	}
}

type fakeEngine struct{ name string }

func (f fakeEngine) Name() string    { return f.name }
func (f fakeEngine) Version() string { return "v0" }
func (f fakeEngine) Format() Format  { return FormatGzip }

func TestByName(t *testing.T) {
	engines := []Engine{fakeEngine{"a"}, fakeEngine{"b"}}
	if e, ok := ByName(engines, "b"); !ok || e.Name() != "b" {
		t.Fatalf("got %v %v", e, ok)
	}
	if _, ok := ByName(engines, "c"); ok {
		t.Fatal("found c")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/`
Expected: build failure.

- [ ] **Step 3: Implement**

`engine/engine.go`:
```go
// Package engine defines the compression engines that comp-prysm searches
// over, and the parameter types recorded in Params.
package engine

import (
	"io"
	"runtime/debug"

	"github.com/draganm/comp-prysm/format"
)

// Format is re-exported from package format.
type Format = format.Format

const (
	FormatNone = format.None
	FormatGzip = format.Gzip
	FormatZstd = format.Zstd
)

// Engine is a compression implementation with a stable name and a version
// that determines its output.
type Engine interface {
	// Name is the stable identifier stored in Params, for example "zlib".
	Name() string
	// Version identifies the implementation build, for example "1.3.2".
	Version() string
	// Format is the container format this engine produces payloads for.
	Format() Format
}

// zlib strategy names.
const (
	StrategyDefault     = "default"
	StrategyFiltered    = "filtered"
	StrategyHuffmanOnly = "huffman_only"
	StrategyRLE         = "rle"
	StrategyFixed       = "fixed"
)

// DeflateParams configures a raw deflate stream.
type DeflateParams struct {
	Level      int    `json:"level"`
	Strategy   string `json:"strategy,omitempty"`    // zlib only
	WindowBits int    `json:"window_bits,omitempty"` // zlib only: 9..15
	MemLevel   int    `json:"mem_level,omitempty"`   // zlib only: 1..9
}

// GzipParams is DeflateParams plus the verbatim gzip header.
type GzipParams struct {
	HeaderB64 string `json:"header_b64"`
	DeflateParams
}

// DeflateEngine produces raw deflate streams (no gzip header or trailer).
type DeflateEngine interface {
	Engine
	// Candidates returns parameter sets grouped by tier, most likely first.
	Candidates(h *format.GzipHeader, uncompressedSize int64) [][]DeflateParams
	// NewWriter returns a writer that compresses into w. Close flushes.
	NewWriter(w io.Writer, p DeflateParams) (io.WriteCloser, error)
}

// ZstdParams configures a zstd frame.
type ZstdParams struct {
	Level         int  `json:"level"`
	WindowLog     int  `json:"window_log,omitempty"` // 0 means the engine default
	Checksum      bool `json:"checksum"`
	ContentSize   bool `json:"content_size"`   // frame header carries the content size
	PledgedSize   bool `json:"pledged_size"`   // encoder knew the source size
	SingleSegment bool `json:"single_segment"` // frame has no window descriptor
	Workers       int  `json:"workers"`        // 0: single-thread path, 1: job-based path
	Long          bool `json:"long,omitempty"` // long distance matching
	EncodeAll     bool `json:"encode_all,omitempty"`
}

// ZstdEngine produces complete zstd frames.
type ZstdEngine interface {
	Engine
	Candidates(h *format.ZstdFrameHeader, uncompressedSize int64) [][]ZstdParams
	NewWriter(w io.Writer, p ZstdParams, uncompressedSize int64) (io.WriteCloser, error)
}

// LevelOrder returns deflate levels 0..9 ordered by likelihood given the gzip
// XFL byte: 2 marks maximum compression, 4 marks fastest.
func LevelOrder(xfl byte) []int {
	switch xfl {
	case 2:
		return []int{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}
	case 4:
		return []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 0}
	default:
		return []int{6, 5, 7, 4, 8, 3, 9, 2, 1, 0}
	}
}

// ModuleVersion returns the version of a dependency module recorded in the
// binary's build info, or "(devel)" when unknown.
func ModuleVersion(path string) string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "(devel)"
	}
	for _, d := range bi.Deps {
		if d.Path != path {
			continue
		}
		if d.Replace != nil {
			return d.Replace.Version
		}
		return d.Version
	}
	return "(devel)"
}

// ByName finds an engine by name.
func ByName(engines []Engine, name string) (Engine, bool) {
	for _, e := range engines {
		if e.Name() == name {
			return e, true
		}
	}
	return nil, false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add engine
git commit -m "Add engine interfaces and parameter types"
```

---

### Task 5: search package: spool, compare writer, deterministic search

**Files:**
- Create: `search/spool.go`, `search/compare.go`, `search/run.go`, `search/spool_test.go`, `search/compare_test.go`, `search/run_test.go`

**Interfaces:**
- Consumes: `engine.Engine`, `engine.DeflateEngine`, `engine.ZstdEngine`, `engine.DeflateParams`, `engine.ZstdParams`, `format.Format`
- Produces:
  - `type Spool struct`; `func NewSpool(dir string, maxMem int64) *Spool`; methods `Write([]byte) (int, error)`, `Size() int64`, `InMemory() bool`, `Reader() io.Reader` (fresh, concurrency-safe), `Close() error`
  - `var ErrMismatch`, `var ErrNoMatch`
  - `type Candidate struct { Engine engine.Engine; Deflate *engine.DeflateParams; Zstd *engine.ZstdParams }`
  - `type Input struct { Format format.Format; Payload func() (io.Reader, error); Concurrent bool; Trailer []byte; Spool *Spool; UncompressedSize int64 }`
  - `type Result struct { Index int; Candidate Candidate; Tried int }`
  - `func Run(ctx context.Context, in *Input, cands []Candidate, parallelism int) (*Result, error)`

- [ ] **Step 1: Write failing spool tests**

`search/spool_test.go`:
```go
package search

import (
	"bytes"
	"io"
	"testing"
)

func TestSpoolStaysInMemoryBelowLimit(t *testing.T) {
	s := NewSpool(t.TempDir(), 100)
	defer s.Close()
	s.Write([]byte("hello "))
	s.Write([]byte("world"))
	if !s.InMemory() || s.Size() != 11 {
		t.Fatalf("in memory %v size %d", s.InMemory(), s.Size())
	}
	got, _ := io.ReadAll(s.Reader())
	if string(got) != "hello world" {
		t.Fatalf("got %q", got)
	}
}

func TestSpoolSpillsToFile(t *testing.T) {
	s := NewSpool(t.TempDir(), 8)
	defer s.Close()
	s.Write([]byte("12345"))
	if !s.InMemory() {
		t.Fatal("spilled too early")
	}
	s.Write([]byte("6789"))
	if s.InMemory() {
		t.Fatal("did not spill")
	}
	s.Write([]byte("abc"))
	if s.Size() != 12 {
		t.Fatalf("size %d", s.Size())
	}
	a, _ := io.ReadAll(s.Reader())
	b, _ := io.ReadAll(s.Reader())
	if string(a) != "123456789abc" || !bytes.Equal(a, b) {
		t.Fatalf("got %q and %q", a, b)
	}
}

func TestSpoolReadersAreIndependent(t *testing.T) {
	s := NewSpool(t.TempDir(), 4)
	defer s.Close()
	s.Write([]byte("abcdefgh"))
	r1, r2 := s.Reader(), s.Reader()
	b1 := make([]byte, 3)
	io.ReadFull(r1, b1)
	b2 := make([]byte, 3)
	io.ReadFull(r2, b2)
	if string(b1) != "abc" || string(b2) != "abc" {
		t.Fatalf("%q %q", b1, b2)
	}
}

func TestSpoolEmpty(t *testing.T) {
	s := NewSpool(t.TempDir(), 4)
	defer s.Close()
	got, _ := io.ReadAll(s.Reader())
	if len(got) != 0 || s.Size() != 0 {
		t.Fatal("expected empty")
	}
}
```

- [ ] **Step 2: Implement the spool**

`search/spool.go`:
```go
// Package search evaluates candidate engine parameters against a reference
// compressed stream.
package search

import (
	"bytes"
	"io"
	"os"
)

// Spool holds uncompressed content for the duration of a search. It starts
// in memory and spills to an unlinked temp file once it exceeds maxMem.
type Spool struct {
	dir    string
	maxMem int64
	buf    []byte
	file   *os.File
	size   int64
}

// NewSpool returns an empty spool that spills to dir above maxMem bytes.
func NewSpool(dir string, maxMem int64) *Spool {
	return &Spool{dir: dir, maxMem: maxMem}
}

func (s *Spool) Write(p []byte) (int, error) {
	if s.file == nil {
		if int64(len(s.buf))+int64(len(p)) <= s.maxMem {
			s.buf = append(s.buf, p...)
			s.size += int64(len(p))
			return len(p), nil
		}
		if err := s.spill(); err != nil {
			return 0, err
		}
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	return n, err
}

func (s *Spool) spill() error {
	f, err := os.CreateTemp(s.dir, "comp-prysm-spool-*")
	if err != nil {
		return err
	}
	// Unlink immediately: the descriptor keeps the data alive and a crash
	// leaves nothing behind.
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(s.buf); err != nil {
		f.Close()
		return err
	}
	s.file = f
	s.buf = nil
	return nil
}

// Size is the number of bytes written so far.
func (s *Spool) Size() int64 { return s.size }

// InMemory reports whether the content is still held in memory.
func (s *Spool) InMemory() bool { return s.file == nil }

// Reader returns a new reader over the whole content. Readers are
// independent and may be used concurrently.
func (s *Spool) Reader() io.Reader {
	if s.file == nil {
		return bytes.NewReader(s.buf)
	}
	return io.NewSectionReader(s.file, 0, s.size)
}

// Close releases the memory or file backing the spool.
func (s *Spool) Close() error {
	s.buf = nil
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
```

Run: `nix develop --command go test ./search/ -run Spool -v` → PASS.

- [ ] **Step 3: Write failing compare writer tests**

`search/compare_test.go`:
```go
package search

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestCompareWriterMatches(t *testing.T) {
	ref := []byte("abcdefghij")
	cw := newCompareWriter(context.Background(), bytes.NewReader(ref))
	for _, chunk := range [][]byte{[]byte("abc"), []byte("defg"), []byte("hij")} {
		if _, err := cw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := cw.AtEOF(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareWriterDiffers(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("abcdef")))
	cw.Write([]byte("abc"))
	_, err := cw.Write([]byte("dXf"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceShort(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("ab")))
	_, err := cw.Write([]byte("abc"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterReferenceLong(t *testing.T) {
	cw := newCompareWriter(context.Background(), bytes.NewReader([]byte("abc")))
	cw.Write([]byte("ab"))
	if err := cw.AtEOF(); !errors.Is(err, ErrMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestCompareWriterCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cw := newCompareWriter(ctx, bytes.NewReader([]byte("abc")))
	if _, err := cw.Write([]byte("a")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 4: Implement the compare writer**

`search/compare.go`:
```go
package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrMismatch reports that a candidate's output differs from the reference.
var ErrMismatch = errors.New("search: output differs from reference")

// compareWriter compares everything written to it against a reference
// reader and fails at the first difference.
type compareWriter struct {
	ctx context.Context
	ref io.Reader
	buf []byte
	n   int64
}

func newCompareWriter(ctx context.Context, ref io.Reader) *compareWriter {
	return &compareWriter{ctx: ctx, ref: ref}
}

func (c *compareWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	if cap(c.buf) < len(p) {
		c.buf = make([]byte, len(p))
	}
	buf := c.buf[:len(p)]
	n, err := io.ReadFull(c.ref, buf)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, fmt.Errorf("%w: reference ended at byte %d", ErrMismatch, c.n+int64(n))
		}
		return 0, err
	}
	if !bytes.Equal(p, buf) {
		off := 0
		for p[off] == buf[off] {
			off++
		}
		return 0, fmt.Errorf("%w: at byte %d", ErrMismatch, c.n+int64(off))
	}
	c.n += int64(len(p))
	return len(p), nil
}

// AtEOF returns nil when the reference has no bytes left.
func (c *compareWriter) AtEOF() error {
	var b [1]byte
	_, err := io.ReadFull(c.ref, b[:])
	switch {
	case err == nil:
		return fmt.Errorf("%w: reference has extra bytes after %d", ErrMismatch, c.n)
	case errors.Is(err, io.EOF):
		return nil
	default:
		return err
	}
}
```

Run: `nix develop --command go test ./search/ -run Compare -v` → PASS.

- [ ] **Step 5: Write failing search tests**

`search/run_test.go`:
```go
package search

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// fakeDeflate "compresses" by prefixing the content with its level byte.
type fakeDeflate struct {
	mu    sync.Mutex
	calls []int
}

func (f *fakeDeflate) Name() string          { return "fake" }
func (f *fakeDeflate) Version() string       { return "1" }
func (f *fakeDeflate) Format() engine.Format { return engine.FormatGzip }
func (f *fakeDeflate) Candidates(*format.GzipHeader, int64) [][]engine.DeflateParams {
	return nil
}

type prefixWriter struct {
	w     io.Writer
	level int
	first bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if !p.first {
		p.first = true
		if _, err := p.w.Write([]byte{byte(p.level)}); err != nil {
			return 0, err
		}
	}
	return p.w.Write(b)
}

func (p *prefixWriter) Close() error { return nil }

func (f *fakeDeflate) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	f.mu.Lock()
	f.calls = append(f.calls, p.Level)
	f.mu.Unlock()
	return &prefixWriter{w: w, level: p.Level}, nil
}

func input(t *testing.T, content []byte, level int, concurrent bool) *Input {
	t.Helper()
	ref := append([]byte{byte(level)}, content...)
	ref = append(ref, "TRAILER"...)
	sp := NewSpool(t.TempDir(), 1<<20)
	sp.Write(content)
	t.Cleanup(func() { sp.Close() })
	return &Input{
		Format:           format.Gzip,
		Payload:          func() (io.Reader, error) { return bytes.NewReader(ref), nil },
		Concurrent:       concurrent,
		Trailer:          []byte("TRAILER"),
		Spool:            sp,
		UncompressedSize: int64(len(content)),
	}
}

func candidates(e engine.Engine, levels ...int) []Candidate {
	var out []Candidate
	for _, l := range levels {
		p := engine.DeflateParams{Level: l}
		out = append(out, Candidate{Engine: e, Deflate: &p})
	}
	return out
}

func TestRunFindsMatchSequentially(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, []byte("content"), 3, false), candidates(e, 0, 1, 2, 3, 4), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != 3 || res.Candidate.Deflate.Level != 3 || res.Tried != 4 {
		t.Fatalf("%+v", res)
	}
}

func TestRunFindsMatchInParallel(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, bytes.Repeat([]byte("x"), 100000), 7, true), candidates(e, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9), 4)
	if err != nil {
		t.Fatal(err)
	}
	if res.Candidate.Deflate.Level != 7 {
		t.Fatalf("%+v", res)
	}
}

func TestRunPicksEarliestOfEqualMatches(t *testing.T) {
	e := &fakeDeflate{}
	// Levels 3 and 3 again: both reproduce; index 2 must win even under parallelism.
	for i := 0; i < 20; i++ {
		res, err := Run(context.Background(), input(t, []byte("content"), 3, true), candidates(e, 0, 1, 3, 3, 3), 4)
		if err != nil {
			t.Fatal(err)
		}
		if res.Index != 2 {
			t.Fatalf("iteration %d: index %d", i, res.Index)
		}
	}
}

func TestRunNoMatch(t *testing.T) {
	e := &fakeDeflate{}
	_, err := Run(context.Background(), input(t, []byte("content"), 9, false), candidates(e, 0, 1, 2), 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
}

func TestRunSequentialWhenNotConcurrent(t *testing.T) {
	e := &fakeDeflate{}
	res, err := Run(context.Background(), input(t, []byte("content"), 2, false), candidates(e, 0, 1, 2, 3), 8)
	if err != nil {
		t.Fatal(err)
	}
	// Sequential search stops at the first match, so level 3 is never tried.
	if res.Tried != 3 {
		t.Fatalf("tried %d", res.Tried)
	}
}

func TestRunCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &fakeDeflate{}
	_, err := Run(ctx, input(t, []byte("content"), 2, true), candidates(e, 0, 1, 2), 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestRunTrailerMismatch(t *testing.T) {
	e := &fakeDeflate{}
	in := input(t, []byte("content"), 3, false)
	in.Trailer = []byte("WRONG!!")
	_, err := Run(context.Background(), in, candidates(e, 3), 1)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 6: Implement the search**

`search/run.go`:
```go
package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// ErrNoMatch reports that no candidate reproduced the reference.
var ErrNoMatch = errors.New("search: no candidate reproduced the input")

// Candidate is one engine with one parameter set. Exactly one of Deflate or
// Zstd is set, matching the engine's format.
type Candidate struct {
	Engine  engine.Engine
	Deflate *engine.DeflateParams
	Zstd    *engine.ZstdParams
}

// Input describes the reference compressed stream and the spooled content.
type Input struct {
	Format format.Format
	// Payload returns a fresh reader over the compressed bytes that follow
	// the header, up to the end of the input.
	Payload func() (io.Reader, error)
	// Concurrent reports whether Payload may be called from several
	// goroutines at once. When false the search is sequential.
	Concurrent bool
	// Trailer is compared after the engine output; nil for zstd.
	Trailer          []byte
	Spool            *Spool
	UncompressedSize int64
}

// Result is a successful search.
type Result struct {
	Index     int
	Candidate Candidate
	Tried     int
}

// Run evaluates candidates in order and returns the earliest one that
// reproduces the input. With parallelism above one and a concurrent input,
// candidates are evaluated by a worker pool; the result is still the
// earliest match in list order.
func Run(ctx context.Context, in *Input, cands []Candidate, parallelism int) (*Result, error) {
	if len(cands) == 0 {
		return nil, fmt.Errorf("%w: no candidates", ErrNoMatch)
	}
	if parallelism < 1 || !in.Concurrent {
		parallelism = 1
	}
	if parallelism > len(cands) {
		parallelism = len(cands)
	}

	var (
		mu       sync.Mutex
		next     int
		winner   = -1
		tried    int
		running  = map[int]context.CancelFunc{}
		firstErr error
		wg       sync.WaitGroup
	)
	for w := 0; w < parallelism; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if ctx.Err() != nil || next >= len(cands) || (winner >= 0 && next > winner) {
					mu.Unlock()
					return
				}
				i := next
				next++
				cctx, cancel := context.WithCancel(ctx)
				running[i] = cancel
				tried++
				mu.Unlock()

				err := evaluate(cctx, in, cands[i])

				mu.Lock()
				delete(running, i)
				cancel()
				switch {
				case err == nil:
					if winner < 0 || i < winner {
						winner = i
						for j, c := range running {
							if j > i {
								c()
							}
						}
					}
				case errors.Is(err, ErrMismatch), errors.Is(err, context.Canceled):
				default:
					if firstErr == nil {
						firstErr = err
					}
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if winner < 0 {
		if firstErr != nil {
			return nil, fmt.Errorf("%w: tried %d candidates; first non-mismatch error: %v", ErrNoMatch, tried, firstErr)
		}
		return nil, fmt.Errorf("%w: tried %d candidates", ErrNoMatch, tried)
	}
	return &Result{Index: winner, Candidate: cands[winner], Tried: tried}, nil
}

// evaluate re-compresses the spool with one candidate and compares the
// output against the reference. It returns nil on an exact match.
func evaluate(ctx context.Context, in *Input, c Candidate) error {
	ref, err := in.Payload()
	if err != nil {
		return err
	}
	cw := newCompareWriter(ctx, ref)
	var w io.WriteCloser
	switch in.Format {
	case format.Gzip:
		e, ok := c.Engine.(engine.DeflateEngine)
		if !ok || c.Deflate == nil {
			return fmt.Errorf("search: %s is not a deflate candidate", c.Engine.Name())
		}
		w, err = e.NewWriter(cw, *c.Deflate)
	case format.Zstd:
		e, ok := c.Engine.(engine.ZstdEngine)
		if !ok || c.Zstd == nil {
			return fmt.Errorf("search: %s is not a zstd candidate", c.Engine.Name())
		}
		w, err = e.NewWriter(cw, *c.Zstd, in.UncompressedSize)
	default:
		return fmt.Errorf("search: unsupported format %q", in.Format)
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, in.Spool.Reader()); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	if len(in.Trailer) > 0 {
		if _, err := cw.Write(in.Trailer); err != nil {
			return err
		}
	}
	return cw.AtEOF()
}
```

- [ ] **Step 7: Run all search tests, with the race detector**

Run: `nix develop --command go test -race ./search/ -v`
Expected: all PASS, no races.

- [ ] **Step 8: Commit**

```bash
git add search
git commit -m "Add spool, compare writer and deterministic candidate search"
```

---

### Task 6: fixtures and enginetest packages

**Files:**
- Create: `fixtures/fixtures.go`, `fixtures/fixtures_test.go`, `enginetest/enginetest.go`

**Interfaces:**
- Consumes: `search.Run`, `search.Input`, `search.NewSpool`, `format.ParseGzipHeader`, `format.ParseZstdFrameHeader`, `engine.DeflateEngine`, `engine.ZstdEngine`
- Produces:
  - `type fixtures.Fixture struct { Name string; Data []byte }`
  - `func fixtures.Small() []Fixture` (empty, tiny, text 64 KiB, zeros 256 KiB, random 16 KiB, mixed 300 KiB)
  - `func fixtures.All() []Fixture` (Small plus json 256 KiB, text 1 MiB, zeros 2 MiB, random 256 KiB, mixed 3 MiB)
  - `func fixtures.Text(n int) []byte`, `JSON(n int) []byte`, `Zeros(n int) []byte`, `Random(n int, seed int64) []byte`, `Mixed(n int) []byte`; all deterministic.
  - `func enginetest.Gzip(t testing.TB, e engine.DeflateEngine, p engine.DeflateParams, data []byte) []byte` builds a complete gzip file (10-byte header with XFL 2 for level 9, 4 for level 1, else 0, OS 255).
  - `func enginetest.Zstd(t testing.TB, e engine.ZstdEngine, p engine.ZstdParams, data []byte) []byte`
  - `func enginetest.RoundTripDeflate(t *testing.T, e engine.DeflateEngine, params []engine.DeflateParams, fx []fixtures.Fixture)`
  - `func enginetest.RoundTripZstd(t *testing.T, e engine.ZstdEngine, params []engine.ZstdParams, fx []fixtures.Fixture)`
  - `func enginetest.Flatten[T any](tiers [][]T) []T`

- [ ] **Step 1: Write the fixtures package and its test**

`fixtures/fixtures.go`:
```go
// Package fixtures provides deterministic sample inputs for tests.
package fixtures

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
)

// Fixture is a named sample input.
type Fixture struct {
	Name string
	Data []byte
}

var words = strings.Fields(`the quick brown fox jumps over the lazy dog while compression
engines search for matching parameters across levels strategies and windows so that every
archive can be rebuilt from its uncompressed content and verified with a blake3 digest`)

// Text returns n bytes of pseudo-English built from a fixed word list.
func Text(n int) []byte {
	r := rand.New(rand.NewSource(1))
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[r.Intn(len(words))])
		if r.Intn(12) == 0 {
			b.WriteString(".\n")
		} else {
			b.WriteByte(' ')
		}
	}
	return b.Bytes()[:n]
}

// JSON returns n bytes of a JSON array of small objects.
func JSON(n int) []byte {
	r := rand.New(rand.NewSource(2))
	var b bytes.Buffer
	b.WriteString("[\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, `  {"id": %d, "name": "%s", "score": %.3f, "tags": ["%s", "%s"]},`+"\n",
			i, words[r.Intn(len(words))], r.Float64()*100, words[r.Intn(len(words))], words[r.Intn(len(words))])
	}
	b.WriteString("]\n")
	return b.Bytes()[:n]
}

// Zeros returns n zero bytes.
func Zeros(n int) []byte { return make([]byte, n) }

// Random returns n pseudo-random bytes from seed.
func Random(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// Mixed returns n bytes alternating text, random and zero segments.
func Mixed(n int) []byte {
	var b bytes.Buffer
	seg := 0
	for b.Len() < n {
		switch seg % 3 {
		case 0:
			b.Write(Text(40000))
		case 1:
			b.Write(Random(7000, int64(seg)))
		case 2:
			b.Write(Zeros(20000))
		}
		seg++
	}
	return b.Bytes()[:n]
}

// Small is a fast set for engine tests.
func Small() []Fixture {
	return []Fixture{
		{"empty", nil},
		{"tiny", []byte("hello, comp-prysm")},
		{"text-64k", Text(64 << 10)},
		{"zeros-256k", Zeros(256 << 10)},
		{"random-16k", Random(16<<10, 3)},
		{"mixed-300k", Mixed(300 << 10)},
	}
}

// All is the full set for end-to-end tests.
func All() []Fixture {
	return append(Small(),
		Fixture{"json-256k", JSON(256 << 10)},
		Fixture{"text-1m", Text(1 << 20)},
		Fixture{"zeros-2m", Zeros(2 << 20)},
		Fixture{"random-256k", Random(256<<10, 4)},
		Fixture{"mixed-3m", Mixed(3 << 20)},
	)
}
```

`fixtures/fixtures_test.go`:
```go
package fixtures

import (
	"bytes"
	"testing"
)

func TestDeterministic(t *testing.T) {
	if !bytes.Equal(Text(1000), Text(1000)) || !bytes.Equal(Mixed(100000), Mixed(100000)) {
		t.Fatal("fixtures are not deterministic")
	}
}

func TestSizes(t *testing.T) {
	for _, f := range All() {
		if f.Name == "empty" && len(f.Data) != 0 {
			t.Fatal("empty is not empty")
		}
	}
	if len(Text(12345)) != 12345 || len(JSON(4321)) != 4321 || len(Mixed(99999)) != 99999 {
		t.Fatal("wrong sizes")
	}
}
```

Run: `nix develop --command go test ./fixtures/` → PASS.

- [ ] **Step 2: Write the enginetest package**

`enginetest/enginetest.go`:
```go
// Package enginetest holds conformance tests shared by all engines.
package enginetest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
	"github.com/draganm/comp-prysm/search"
)

// Flatten concatenates tiers in order.
func Flatten[T any](tiers [][]T) []T {
	var out []T
	for _, t := range tiers {
		out = append(out, t...)
	}
	return out
}

// Gzip builds a complete gzip file from e with parameters p.
func Gzip(t testing.TB, e engine.DeflateEngine, p engine.DeflateParams, data []byte) []byte {
	t.Helper()
	var xfl byte
	switch p.Level {
	case 9:
		xfl = 2
	case 1:
		xfl = 4
	}
	var buf bytes.Buffer
	buf.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, xfl, 255})
	w, err := e.NewWriter(&buf, p)
	if err != nil {
		t.Fatalf("%s %+v: %v", e.Name(), p, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc32.ChecksumIEEE(data))
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(len(data)))
	buf.Write(trailer[:])
	return buf.Bytes()
}

// Zstd builds a complete zstd frame from e with parameters p.
func Zstd(t testing.TB, e engine.ZstdEngine, p engine.ZstdParams, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := e.NewWriter(&buf, p, int64(len(data)))
	if err != nil {
		t.Fatalf("%s %+v: %v", e.Name(), p, err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func spool(t testing.TB, data []byte) *search.Spool {
	t.Helper()
	sp := search.NewSpool(t.TempDir(), 1<<20)
	if _, err := sp.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sp.Close() })
	return sp
}

// RoundTripDeflate compresses every fixture with every parameter set, then
// checks the search over all of e's candidates finds a reproducing one.
func RoundTripDeflate(t *testing.T, e engine.DeflateEngine, params []engine.DeflateParams, fx []fixtures.Fixture) {
	for _, f := range fx {
		for _, p := range params {
			t.Run(fmt.Sprintf("%s/%+v", f.Name, p), func(t *testing.T) {
				file := Gzip(t, e, p, f.Data)
				hdr, err := format.ParseGzipHeader(bufio.NewReader(bytes.NewReader(file)))
				if err != nil {
					t.Fatal(err)
				}
				var cands []search.Candidate
				for _, c := range Flatten(e.Candidates(hdr, int64(len(f.Data)))) {
					c := c
					cands = append(cands, search.Candidate{Engine: e, Deflate: &c})
				}
				in := &search.Input{
					Format:           format.Gzip,
					Payload:          func() (io.Reader, error) { return bytes.NewReader(file[len(hdr.Raw):]), nil },
					Concurrent:       true,
					Trailer:          file[len(file)-8:],
					Spool:            spool(t, f.Data),
					UncompressedSize: int64(len(f.Data)),
				}
				res, err := search.Run(context.Background(), in, cands, 4)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				again := Gzip(t, e, *res.Candidate.Deflate, f.Data)
				if !bytes.Equal(again, file) {
					t.Fatalf("found %+v but it does not reproduce the file", *res.Candidate.Deflate)
				}
			})
		}
	}
}

// RoundTripZstd is the zstd counterpart of RoundTripDeflate.
func RoundTripZstd(t *testing.T, e engine.ZstdEngine, params []engine.ZstdParams, fx []fixtures.Fixture) {
	for _, f := range fx {
		for _, p := range params {
			t.Run(fmt.Sprintf("%s/%+v", f.Name, p), func(t *testing.T) {
				file := Zstd(t, e, p, f.Data)
				hdr, err := format.ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(file)))
				if err != nil {
					t.Fatal(err)
				}
				var cands []search.Candidate
				for _, c := range Flatten(e.Candidates(hdr, int64(len(f.Data)))) {
					c := c
					cands = append(cands, search.Candidate{Engine: e, Zstd: &c})
				}
				in := &search.Input{
					Format:           format.Zstd,
					Payload:          func() (io.Reader, error) { return bytes.NewReader(file), nil },
					Concurrent:       true,
					Spool:            spool(t, f.Data),
					UncompressedSize: int64(len(f.Data)),
				}
				res, err := search.Run(context.Background(), in, cands, 4)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				again := Zstd(t, e, *res.Candidate.Zstd, f.Data)
				if !bytes.Equal(again, file) {
					t.Fatalf("found %+v but it does not reproduce the file", *res.Candidate.Zstd)
				}
			})
		}
	}
}
```

Run: `nix develop --command go vet ./fixtures/ ./enginetest/` → clean.

- [ ] **Step 3: Commit**

```bash
git add fixtures enginetest
git commit -m "Add test fixtures and shared engine conformance tests"
```

---

### Task 7: go-flate engine

**Files:**
- Create: `engine/goflate/goflate.go`, `engine/goflate/goflate_test.go`

**Interfaces:**
- Consumes: `engine.DeflateEngine`, `engine.LevelOrder`, `enginetest.RoundTripDeflate`, `fixtures.Small`
- Produces: `func goflate.New() *Engine`; `Engine` implements `engine.DeflateEngine` with `Name() == "go-flate"`, `Version() == runtime.Version()`.

- [ ] **Step 1: Write failing tests**

`engine/goflate/goflate_test.go`:
```go
package goflate

import (
	"runtime"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "go-flate" || e.Version() != runtime.Version() || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s %s", e.Name(), e.Version(), e.Format())
	}
}

func TestCandidatesFollowXFL(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 0)
	if len(tiers) != 1 || len(tiers[0]) != 11 || tiers[0][0].Level != 9 || tiers[0][10].Level != -2 {
		t.Fatalf("%+v", tiers)
	}
}

func TestRejectsZlibOnlyParams(t *testing.T) {
	if _, err := New().NewWriter(nil, engine.DeflateParams{Level: 6, MemLevel: 8}); err == nil {
		t.Fatal("expected error for mem_level")
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/goflate/` → build failure.

- [ ] **Step 3: Implement**

`engine/goflate/goflate.go`:
```go
// Package goflate is the Go standard library deflate engine.
package goflate

import (
	"compress/flate"
	"errors"
	"io"
	"runtime"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// Engine produces raw deflate streams with compress/flate.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "go-flate" }
func (*Engine) Version() string       { return runtime.Version() }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns levels 0..9 ordered by the XFL hint, then HuffmanOnly.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	var tier []engine.DeflateParams
	for _, l := range engine.LevelOrder(h.XFL) {
		tier = append(tier, engine.DeflateParams{Level: l})
	}
	tier = append(tier, engine.DeflateParams{Level: flate.HuffmanOnly})
	return [][]engine.DeflateParams{tier}
}

func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Strategy != "" || p.WindowBits != 0 || p.MemLevel != 0 {
		return nil, errors.New("go-flate: strategy, window_bits and mem_level are not supported")
	}
	return flate.NewWriter(w, p.Level)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/goflate/`
Expected: PASS (66 subtests in RoundTrip).

- [ ] **Step 5: Commit**

```bash
git add engine/goflate
git commit -m "Add go-flate engine"
```

---

### Task 8: klauspost-flate engine

**Files:**
- Create: `engine/kpflate/kpflate.go`, `engine/kpflate/kpflate_test.go`

**Interfaces:**
- Consumes: same as Task 7, plus `engine.ModuleVersion`
- Produces: `func kpflate.New() *Engine`; `Name() == "klauspost-flate"`, `Version() == engine.ModuleVersion("github.com/klauspost/compress")`.

- [ ] **Step 1: Write failing tests**

`engine/kpflate/kpflate_test.go`:
```go
package kpflate

import (
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "klauspost-flate" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "v1.20.0" {
		t.Fatalf("version %q; update this test when bumping klauspost/compress", v)
	}
}

func TestCandidates(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 4}, 0)
	if len(tiers) != 1 || len(tiers[0]) != 11 || tiers[0][0].Level != 1 || tiers[0][10].Level != -2 {
		t.Fatalf("%+v", tiers)
	}
}

func TestRoundTrip(t *testing.T) {
	e := New()
	enginetest.RoundTripDeflate(t, e, enginetest.Flatten(e.Candidates(&format.GzipHeader{}, 0)), fixtures.Small())
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/kpflate/` → build failure.

- [ ] **Step 3: Implement**

`engine/kpflate/kpflate.go`:
```go
// Package kpflate is the klauspost/compress deflate engine.
package kpflate

import (
	"errors"
	"io"

	"github.com/klauspost/compress/flate"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

const modulePath = "github.com/klauspost/compress"

// Engine produces raw deflate streams with klauspost/compress/flate.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "klauspost-flate" }
func (*Engine) Version() string       { return engine.ModuleVersion(modulePath) }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns levels 0..9 ordered by the XFL hint, then HuffmanOnly.
// klauspost's DefaultCompression (-1) is level 5 and is not listed twice.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	var tier []engine.DeflateParams
	for _, l := range engine.LevelOrder(h.XFL) {
		tier = append(tier, engine.DeflateParams{Level: l})
	}
	tier = append(tier, engine.DeflateParams{Level: flate.HuffmanOnly})
	return [][]engine.DeflateParams{tier}
}

func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	if p.Strategy != "" || p.WindowBits != 0 || p.MemLevel != 0 {
		return nil, errors.New("klauspost-flate: strategy, window_bits and mem_level are not supported")
	}
	return flate.NewWriter(w, p.Level)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/kpflate/` → PASS. If `TestIdentity` reports `(devel)`, the module is not in the test binary's build info: confirm `go.mod` lists `github.com/klauspost/compress v1.20.0` as a direct requirement (run `go mod tidy`).

- [ ] **Step 5: Commit**

```bash
git add engine/kpflate go.mod go.sum
git commit -m "Add klauspost-flate engine"
```

---

### Task 9: zlib engine (cgo)

**Files:**
- Create: `engine/zlib/zlib.go`, `engine/zlib/zlib_test.go`

**Interfaces:**
- Consumes: `engine.DeflateEngine`, `engine.Strategy*`, `engine.LevelOrder`, `enginetest`
- Produces: `func zlib.New() *Engine`; `Name() == "zlib"`, `Version()` from `zlibVersion()`. Package has `//go:build cgo`.

- [ ] **Step 1: Write failing tests**

`engine/zlib/zlib_test.go`:
```go
//go:build cgo

package zlib

import (
	"bytes"
	"compress/flate"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "zlib" || e.Format() != engine.FormatGzip {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "1.3.2" {
		t.Fatalf("zlib version %q, want 1.3.2 from the flake", v)
	}
}

func TestCandidateTiers(t *testing.T) {
	tiers := New().Candidates(&format.GzipHeader{XFL: 2}, 0)
	if len(tiers) != 3 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 10 || tiers[0][0] != (engine.DeflateParams{Level: 9, Strategy: "default", WindowBits: 15, MemLevel: 8}) {
		t.Fatalf("tier 1: %+v", tiers[0])
	}
	if len(tiers[1]) != 9*4 {
		t.Fatalf("tier 2 has %d", len(tiers[1]))
	}
	if len(tiers[2]) != 9*(9*7-1) {
		t.Fatalf("tier 3 has %d", len(tiers[2]))
	}
}

func TestOutputIsValidDeflate(t *testing.T) {
	data := fixtures.Text(100000)
	for _, p := range []engine.DeflateParams{
		{Level: 0, Strategy: "default", WindowBits: 15, MemLevel: 8},
		{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8},
		{Level: 9, Strategy: "huffman_only", WindowBits: 15, MemLevel: 8},
		{Level: 3, Strategy: "rle", WindowBits: 9, MemLevel: 1},
	} {
		var buf bytes.Buffer
		w, err := New().NewWriter(&buf, p)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		back, err := io.ReadAll(flate.NewReader(&buf))
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("%+v: decode failed: %v", p, err)
		}
	}
}

func TestParamValidation(t *testing.T) {
	for _, p := range []engine.DeflateParams{
		{Level: 10}, {Level: -1}, {Level: 5, Strategy: "bogus"}, {Level: 5, WindowBits: 8}, {Level: 5, MemLevel: 10},
	} {
		if _, err := New().NewWriter(io.Discard, p); err == nil {
			t.Errorf("%+v: expected error", p)
		}
	}
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestPropagatesWriteError(t *testing.T) {
	fw := &failWriter{}
	w, err := New().NewWriter(fw, engine.DeflateParams{Level: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(fixtures.Random(1<<20, 9))
	if err := w.Close(); err == nil {
		t.Fatal("expected error")
	}
	if fw.n != 1 {
		t.Fatalf("kept writing after failure: %d writes", fw.n)
	}
}

func TestRoundTripTier1And2(t *testing.T) {
	e := New()
	tiers := e.Candidates(&format.GzipHeader{}, 0)
	enginetest.RoundTripDeflate(t, e, append(tiers[0], tiers[1]...), fixtures.Small())
}

func TestRoundTripTier3Sample(t *testing.T) {
	e := New()
	var sample []engine.DeflateParams
	for _, l := range []int{1, 6, 9} {
		for _, ml := range []int{1, 5, 9} {
			for _, wb := range []int{9, 12, 15} {
				sample = append(sample, engine.DeflateParams{Level: l, Strategy: "default", WindowBits: wb, MemLevel: ml})
			}
		}
	}
	enginetest.RoundTripDeflate(t, e, sample, fixtures.Small()[2:4])
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/zlib/` → build failure.

- [ ] **Step 3: Implement**

`engine/zlib/zlib.go`:
```go
//go:build cgo

// Package zlib is the cgo engine over the system zlib, producing raw
// deflate streams.
package zlib

/*
#cgo pkg-config: zlib
#include <stdlib.h>
#include <string.h>
#include <zlib.h>

// deflateInit2 is a macro; wrap it so cgo can call it.
static int cp_deflate_init(z_streamp s, int level, int windowBits, int memLevel, int strategy) {
	return deflateInit2(s, level, Z_DEFLATED, windowBits, memLevel, strategy);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

const bufSize = 64 << 10

var strategies = map[string]C.int{
	engine.StrategyDefault:     C.Z_DEFAULT_STRATEGY,
	engine.StrategyFiltered:    C.Z_FILTERED,
	engine.StrategyHuffmanOnly: C.Z_HUFFMAN_ONLY,
	engine.StrategyRLE:         C.Z_RLE,
	engine.StrategyFixed:       C.Z_FIXED,
}

var searchStrategies = []string{engine.StrategyFiltered, engine.StrategyHuffmanOnly, engine.StrategyRLE, engine.StrategyFixed}

// Engine produces raw deflate streams with zlib.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "zlib" }
func (*Engine) Version() string       { return C.GoString(C.zlibVersion()) }
func (*Engine) Format() engine.Format { return engine.FormatGzip }

// Candidates returns three tiers: default strategy at levels 0..9; other
// strategies at levels 1..9; then memory level and window bits variations.
func (*Engine) Candidates(h *format.GzipHeader, _ int64) [][]engine.DeflateParams {
	levels := engine.LevelOrder(h.XFL)
	var t1, t2, t3 []engine.DeflateParams
	for _, l := range levels {
		t1 = append(t1, engine.DeflateParams{Level: l, Strategy: engine.StrategyDefault, WindowBits: 15, MemLevel: 8})
	}
	for _, s := range searchStrategies {
		for _, l := range levels {
			if l == 0 {
				continue // stored blocks ignore the strategy
			}
			t2 = append(t2, engine.DeflateParams{Level: l, Strategy: s, WindowBits: 15, MemLevel: 8})
		}
	}
	for _, l := range levels {
		if l == 0 {
			continue
		}
		for ml := 9; ml >= 1; ml-- {
			for wb := 15; wb >= 9; wb-- {
				if ml == 8 && wb == 15 {
					continue // already in tier 1
				}
				t3 = append(t3, engine.DeflateParams{Level: l, Strategy: engine.StrategyDefault, WindowBits: wb, MemLevel: ml})
			}
		}
	}
	return [][]engine.DeflateParams{t1, t2, t3}
}

type writer struct {
	w      io.Writer
	strm   *C.z_stream
	in     unsafe.Pointer
	out    unsafe.Pointer
	err    error
	closed bool
}

func (*Engine) NewWriter(w io.Writer, p engine.DeflateParams) (io.WriteCloser, error) {
	strategy := p.Strategy
	if strategy == "" {
		strategy = engine.StrategyDefault
	}
	st, ok := strategies[strategy]
	if !ok {
		return nil, fmt.Errorf("zlib: unknown strategy %q", p.Strategy)
	}
	wb, ml := p.WindowBits, p.MemLevel
	if wb == 0 {
		wb = 15
	}
	if ml == 0 {
		ml = 8
	}
	if p.Level < 0 || p.Level > 9 {
		return nil, fmt.Errorf("zlib: level %d out of range 0..9", p.Level)
	}
	if wb < 9 || wb > 15 {
		return nil, fmt.Errorf("zlib: window_bits %d out of range 9..15", wb)
	}
	if ml < 1 || ml > 9 {
		return nil, fmt.Errorf("zlib: mem_level %d out of range 1..9", ml)
	}
	strm := (*C.z_stream)(C.calloc(1, C.size_t(unsafe.Sizeof(C.z_stream{}))))
	if strm == nil {
		return nil, errors.New("zlib: out of memory")
	}
	// Negative window bits selects raw deflate without the zlib wrapper.
	if rc := C.cp_deflate_init(strm, C.int(p.Level), C.int(-wb), C.int(ml), st); rc != C.Z_OK {
		C.free(unsafe.Pointer(strm))
		return nil, fmt.Errorf("zlib: deflateInit2 returned %d", rc)
	}
	z := &writer{w: w, strm: strm, in: C.malloc(bufSize), out: C.malloc(bufSize)}
	z.resetOut()
	return z, nil
}

func (z *writer) resetOut() {
	z.strm.next_out = (*C.Bytef)(z.out)
	z.strm.avail_out = bufSize
}

// flushOut writes whatever deflate produced and resets the output buffer.
func (z *writer) flushOut() error {
	produced := bufSize - int(z.strm.avail_out)
	if produced > 0 {
		if _, err := z.w.Write(unsafe.Slice((*byte)(z.out), produced)); err != nil {
			return err
		}
	}
	z.resetOut()
	return nil
}

func (z *writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("zlib: write after close")
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > bufSize {
			n = bufSize
		}
		C.memcpy(z.in, unsafe.Pointer(&p[0]), C.size_t(n))
		z.strm.next_in = (*C.Bytef)(z.in)
		z.strm.avail_in = C.uInt(n)
		for z.strm.avail_in > 0 {
			rc := C.deflate(z.strm, C.Z_NO_FLUSH)
			if rc != C.Z_OK && rc != C.Z_BUF_ERROR {
				z.err = fmt.Errorf("zlib: deflate returned %d", rc)
				return total, z.err
			}
			if err := z.flushOut(); err != nil {
				z.err = err
				return total, err
			}
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (z *writer) Close() error {
	if z.closed {
		return z.err
	}
	z.closed = true
	defer z.free()
	if z.err != nil {
		return z.err
	}
	z.strm.avail_in = 0
	for {
		rc := C.deflate(z.strm, C.Z_FINISH)
		if err := z.flushOut(); err != nil {
			z.err = err
			return err
		}
		if rc == C.Z_STREAM_END {
			return nil
		}
		if rc != C.Z_OK && rc != C.Z_BUF_ERROR {
			z.err = fmt.Errorf("zlib: deflate(Z_FINISH) returned %d", rc)
			return z.err
		}
	}
}

func (z *writer) free() {
	C.deflateEnd(z.strm)
	C.free(z.in)
	C.free(z.out)
	C.free(unsafe.Pointer(z.strm))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/zlib/`
Expected: PASS. If the build fails with `zlib.h not found`, pkg-config is not resolving inside the shell: re-check Task 1 step 2. If `TestIdentity` reports a version other than 1.3.2, the linker picked an ambient zlib (macOS SDK): run `nix develop --command bash -c 'pkg-config --cflags --libs zlib'` and confirm both point into `/nix/store`.

- [ ] **Step 5: Commit**

```bash
git add engine/zlib
git commit -m "Add cgo zlib engine"
```

---

### Task 10: libzstd engine (cgo)

**Files:**
- Create: `engine/libzstd/libzstd.go`, `engine/libzstd/libzstd_test.go`

**Interfaces:**
- Consumes: `engine.ZstdEngine`, `enginetest.RoundTripZstd`, `enginetest.Zstd`
- Produces: `func libzstd.New() *Engine`; `Name() == "libzstd"`, `Version()` from `ZSTD_versionString()`. Package has `//go:build cgo`.

- [ ] **Step 1: Write failing tests**

`engine/libzstd/libzstd_test.go`:
```go
//go:build cgo

package libzstd

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "libzstd" || e.Format() != engine.FormatZstd {
		t.Fatalf("%s %s", e.Name(), e.Format())
	}
	if v := e.Version(); v != "1.5.7" {
		t.Fatalf("libzstd version %q, want 1.5.7 from the flake", v)
	}
}

func TestCandidateTiers(t *testing.T) {
	// Header with content size: pledged is fixed to true.
	tiers := New().Candidates(&format.ZstdFrameHeader{Checksum: true, HasContentSize: true, WindowLog: 21}, 1000)
	if len(tiers) != 2 {
		t.Fatalf("%d tiers", len(tiers))
	}
	if len(tiers[0]) != 19*2 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	first := tiers[0][0]
	if first.Level != 3 || !first.Checksum || !first.ContentSize || !first.PledgedSize || first.Workers != 0 || first.WindowLog != 0 {
		t.Fatalf("first candidate %+v", first)
	}
	// Without content size both pledged values are tried.
	tiers = New().Candidates(&format.ZstdFrameHeader{WindowLog: 27}, 1000)
	if len(tiers[0]) != 19*2*2 {
		t.Fatalf("tier 1 has %d", len(tiers[0]))
	}
	long := 0
	for _, p := range tiers[1] {
		if p.Long {
			long++
		}
	}
	if long == 0 {
		t.Fatal("window log 27 should add long mode candidates")
	}
}

func TestOutputDecodes(t *testing.T) {
	data := fixtures.Mixed(400000)
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	for _, p := range []engine.ZstdParams{
		{Level: 1, Checksum: true, ContentSize: true, PledgedSize: true},
		{Level: 19, Checksum: false},
		{Level: 3, Workers: 1, Checksum: true},
		{Level: -5},
		{Level: 5, Long: true, WindowLog: 27},
	} {
		frame := enginetest.Zstd(t, New(), p, data)
		back, err := dec.DecodeAll(frame, nil)
		if err != nil || !bytes.Equal(back, data) {
			t.Fatalf("%+v: %v", p, err)
		}
	}
}

func TestPledgedSizeSetsHeader(t *testing.T) {
	data := fixtures.Text(5000)
	frame := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, ContentSize: true, PledgedSize: true}, data)
	h, err := format.ParseZstdFrameHeader(bufioReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	if !h.HasContentSize || h.ContentSize != 5000 || !h.SingleSegment {
		t.Fatalf("%+v", h)
	}
	frame = enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3}, data)
	h, _ = format.ParseZstdFrameHeader(bufioReader(frame))
	if h.HasContentSize || h.SingleSegment {
		t.Fatalf("streamed: %+v", h)
	}
}

func TestWorkersOutputIndependentOfCount(t *testing.T) {
	data := fixtures.Mixed(8 << 20)
	one := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 1}, data)
	four := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 4}, data)
	if !bytes.Equal(one, four) {
		t.Fatal("libzstd output depends on the number of workers; the Workers parameter must record the real count")
	}
	zero := enginetest.Zstd(t, New(), engine.ZstdParams{Level: 3, Workers: 0}, data)
	t.Logf("single-thread and job-based outputs equal: %v", bytes.Equal(zero, one))
}

type failWriter struct{ n int }

func (f *failWriter) Write(p []byte) (int, error) {
	f.n++
	return 0, io.ErrClosedPipe
}

func TestPropagatesWriteError(t *testing.T) {
	fw := &failWriter{}
	w, err := New().NewWriter(fw, engine.ZstdParams{Level: 1}, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(fixtures.Random(2<<20, 5))
	if err := w.Close(); err == nil {
		t.Fatal("expected error")
	}
	if fw.n != 1 {
		t.Fatalf("kept writing after failure: %d writes", fw.n)
	}
}

func TestRoundTrip(t *testing.T) {
	var params []engine.ZstdParams
	for _, l := range []int{1, 3, 9, 19, -3} {
		for _, w := range []int{0, 1} {
			params = append(params,
				engine.ZstdParams{Level: l, Workers: w, Checksum: true, ContentSize: true, PledgedSize: true},
				engine.ZstdParams{Level: l, Workers: w},
			)
		}
	}
	params = append(params, engine.ZstdParams{Level: 5, Long: true, WindowLog: 27, Checksum: true})
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}
```

Add the helper at the bottom of the test file:
```go
func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
```
and add `"bufio"` to the imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/libzstd/` → build failure.

- [ ] **Step 3: Implement**

`engine/libzstd/libzstd.go`:
```go
//go:build cgo

// Package libzstd is the cgo engine over the system libzstd.
package libzstd

/*
#cgo pkg-config: libzstd
#include <stdlib.h>
#include <string.h>
#include <zstd.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// Engine produces zstd frames with libzstd.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "libzstd" }
func (*Engine) Version() string       { return C.GoString(C.ZSTD_versionString()) }
func (*Engine) Format() engine.Format { return engine.FormatZstd }

var tier1Levels = []int{3, 1, 2, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
var tier2Levels = []int{20, 21, 22, -1, -2, -3, -4, -5, -6, -7}

// Candidates returns two tiers. Header fields that are a direct function of
// the parameters (checksum, content size, single segment) are copied, not
// searched.
func (*Engine) Candidates(h *format.ZstdFrameHeader, _ int64) [][]engine.ZstdParams {
	base := engine.ZstdParams{Checksum: h.Checksum, ContentSize: h.HasContentSize, SingleSegment: h.SingleSegment}
	pledged := []bool{true}
	if !h.HasContentSize {
		pledged = []bool{false, true}
	}
	expand := func(levels []int, mutate func(*engine.ZstdParams)) []engine.ZstdParams {
		var out []engine.ZstdParams
		for _, l := range levels {
			for _, w := range []int{0, 1} {
				for _, pl := range pledged {
					p := base
					p.Level, p.Workers, p.PledgedSize = l, w, pl
					if mutate != nil {
						mutate(&p)
					}
					out = append(out, p)
				}
			}
		}
		return out
	}
	t1 := expand(tier1Levels, nil)
	t2 := expand(tier2Levels, nil)
	all := append(append([]int{}, tier1Levels...), 20, 21, 22)
	if h.WindowLog >= 27 {
		t2 = append(t2, expand(all, func(p *engine.ZstdParams) { p.Long = true; p.WindowLog = h.WindowLog })...)
	}
	if h.WindowLog > 0 {
		t2 = append(t2, expand(tier1Levels, func(p *engine.ZstdParams) { p.WindowLog = h.WindowLog })...)
	}
	return [][]engine.ZstdParams{t1, t2}
}

type writer struct {
	w      io.Writer
	cctx   *C.ZSTD_CCtx
	in     unsafe.Pointer
	inCap  int
	out    unsafe.Pointer
	outCap int
	err    error
	closed bool
}

func setParam(cctx *C.ZSTD_CCtx, param C.ZSTD_cParameter, v int) error {
	rc := C.ZSTD_CCtx_setParameter(cctx, param, C.int(v))
	if C.ZSTD_isError(rc) != 0 {
		return fmt.Errorf("libzstd: set parameter %d to %d: %s", int(param), v, C.GoString(C.ZSTD_getErrorName(rc)))
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (*Engine) NewWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	if p.EncodeAll {
		return nil, errors.New("libzstd: encode_all is not a libzstd parameter")
	}
	cctx := C.ZSTD_createCCtx()
	if cctx == nil {
		return nil, errors.New("libzstd: ZSTD_createCCtx failed")
	}
	fail := func(err error) (io.WriteCloser, error) {
		C.ZSTD_freeCCtx(cctx)
		return nil, err
	}
	steps := []struct {
		param C.ZSTD_cParameter
		value int
		when  bool
	}{
		{C.ZSTD_c_compressionLevel, p.Level, true},
		{C.ZSTD_c_checksumFlag, boolInt(p.Checksum), true},
		{C.ZSTD_c_contentSizeFlag, boolInt(p.ContentSize), true},
		{C.ZSTD_c_windowLog, p.WindowLog, p.WindowLog != 0},
		{C.ZSTD_c_enableLongDistanceMatching, 1, p.Long},
		{C.ZSTD_c_nbWorkers, p.Workers, p.Workers > 0},
	}
	for _, s := range steps {
		if !s.when {
			continue
		}
		if err := setParam(cctx, s.param, s.value); err != nil {
			return fail(err)
		}
	}
	if p.PledgedSize {
		if rc := C.ZSTD_CCtx_setPledgedSrcSize(cctx, C.ulonglong(uncompressedSize)); C.ZSTD_isError(rc) != 0 {
			return fail(fmt.Errorf("libzstd: pledge size: %s", C.GoString(C.ZSTD_getErrorName(rc))))
		}
	}
	z := &writer{w: w, cctx: cctx, inCap: int(C.ZSTD_CStreamInSize()), outCap: int(C.ZSTD_CStreamOutSize())}
	z.in = C.malloc(C.size_t(z.inCap))
	z.out = C.malloc(C.size_t(z.outCap))
	return z, nil
}

func (z *writer) emit(n C.size_t) error {
	if n == 0 {
		return nil
	}
	_, err := z.w.Write(unsafe.Slice((*byte)(z.out), int(n)))
	return err
}

func (z *writer) Write(p []byte) (int, error) {
	if z.err != nil {
		return 0, z.err
	}
	if z.closed {
		return 0, errors.New("libzstd: write after close")
	}
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > z.inCap {
			n = z.inCap
		}
		C.memcpy(z.in, unsafe.Pointer(&p[0]), C.size_t(n))
		in := C.ZSTD_inBuffer{src: z.in, size: C.size_t(n), pos: 0}
		for in.pos < in.size {
			out := C.ZSTD_outBuffer{dst: z.out, size: C.size_t(z.outCap), pos: 0}
			rc := C.ZSTD_compressStream2(z.cctx, &out, &in, C.ZSTD_e_continue)
			if C.ZSTD_isError(rc) != 0 {
				z.err = fmt.Errorf("libzstd: compress: %s", C.GoString(C.ZSTD_getErrorName(rc)))
				return total, z.err
			}
			if err := z.emit(out.pos); err != nil {
				z.err = err
				return total, err
			}
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (z *writer) Close() error {
	if z.closed {
		return z.err
	}
	z.closed = true
	defer z.free()
	if z.err != nil {
		return z.err
	}
	in := C.ZSTD_inBuffer{src: z.in, size: 0, pos: 0}
	for {
		out := C.ZSTD_outBuffer{dst: z.out, size: C.size_t(z.outCap), pos: 0}
		rc := C.ZSTD_compressStream2(z.cctx, &out, &in, C.ZSTD_e_end)
		if C.ZSTD_isError(rc) != 0 {
			z.err = fmt.Errorf("libzstd: finish: %s", C.GoString(C.ZSTD_getErrorName(rc)))
			return z.err
		}
		if err := z.emit(out.pos); err != nil {
			z.err = err
			return err
		}
		if rc == 0 {
			return nil
		}
	}
}

func (z *writer) free() {
	C.ZSTD_freeCCtx(z.cctx)
	C.free(z.in)
	C.free(z.out)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/libzstd/`
Expected: PASS. Two outcomes need a decision rather than a workaround, so stop and report if they occur:
- `TestWorkersOutputIndependentOfCount` fails: output depends on the worker count. Report; do not change the assertion.
- `NewWriter` fails on `ZSTD_c_nbWorkers` with "parameter unsupported": the flake's libzstd lacks multithreading. Report.

- [ ] **Step 5: Commit**

```bash
git add engine/libzstd
git commit -m "Add cgo libzstd engine"
```

---

### Task 11: klauspost-zstd engine

**Files:**
- Create: `engine/kpzstd/kpzstd.go`, `engine/kpzstd/kpzstd_test.go`

**Interfaces:**
- Consumes: `engine.ZstdEngine`, `engine.ModuleVersion`, `enginetest`
- Produces: `func kpzstd.New() *Engine`; `Name() == "klauspost-zstd"`. Levels map to klauspost `EncoderLevel`: 1 fastest, 2 default, 3 better, 4 best. Single-segment frames and `EncodeAll` candidates buffer the input in memory, capped at 1 GiB.

- [ ] **Step 1: Write failing tests**

`engine/kpzstd/kpzstd_test.go`:
```go
package kpzstd

import (
	"bufio"
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
	"github.com/draganm/comp-prysm/format"
)

func header(t *testing.T, frame []byte) *format.ZstdFrameHeader {
	t.Helper()
	h, err := format.ParseZstdFrameHeader(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestIdentity(t *testing.T) {
	e := New()
	if e.Name() != "klauspost-zstd" || e.Format() != engine.FormatZstd || e.Version() != "v1.20.0" {
		t.Fatalf("%s %s %s", e.Name(), e.Format(), e.Version())
	}
}

func TestCandidatesFromStreamedHeader(t *testing.T) {
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderConcurrency(1))
	enc.Write(fixtures.Text(100000))
	enc.Close()
	tiers := New().Candidates(header(t, buf.Bytes()), 100000)
	if len(tiers) != 1 || len(tiers[0]) != 4 {
		t.Fatalf("%+v", tiers)
	}
	if p := tiers[0][0]; p.Level != 2 || p.ContentSize || p.SingleSegment || p.EncodeAll || !p.Checksum {
		t.Fatalf("first %+v", p)
	}
}

func TestCandidatesFromEncodeAllHeader(t *testing.T) {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	frame := enc.EncodeAll(fixtures.Text(1000), nil)
	tiers := New().Candidates(header(t, frame), 1000)
	if p := tiers[0][0]; !p.SingleSegment || !p.ContentSize {
		t.Fatalf("first %+v", p)
	}
}

func TestCandidatesRejectNonPowerOfTwoWindow(t *testing.T) {
	h := &format.ZstdFrameHeader{WindowLog: 20, WindowSize: (1 << 20) + (1 << 17)}
	if tiers := New().Candidates(h, 0); len(tiers) != 0 {
		t.Fatalf("expected no candidates, got %+v", tiers)
	}
}

func TestConcurrencyDoesNotChangeOutput(t *testing.T) {
	data := fixtures.Mixed(8 << 20)
	var one, four bytes.Buffer
	e1, _ := zstd.NewWriter(&one, zstd.WithEncoderConcurrency(1))
	e1.Write(data)
	e1.Close()
	e4, _ := zstd.NewWriter(&four, zstd.WithEncoderConcurrency(4))
	e4.Write(data)
	e4.Close()
	if !bytes.Equal(one.Bytes(), four.Bytes()) {
		t.Fatal("klauspost zstd output depends on concurrency; files made with concurrency > 1 will not be reproducible")
	}
}

func TestRoundTrip(t *testing.T) {
	var params []engine.ZstdParams
	for l := 1; l <= 4; l++ {
		params = append(params,
			engine.ZstdParams{Level: l, Checksum: true},
			engine.ZstdParams{Level: l, Checksum: false, ContentSize: true, PledgedSize: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, SingleSegment: true},
			engine.ZstdParams{Level: l, Checksum: true, ContentSize: true, PledgedSize: true, EncodeAll: true},
		)
	}
	enginetest.RoundTripZstd(t, New(), params, fixtures.Small())
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test ./engine/kpzstd/` → build failure.

- [ ] **Step 3: Implement**

`engine/kpzstd/kpzstd.go`:
```go
// Package kpzstd is the klauspost/compress zstd engine.
package kpzstd

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

const (
	modulePath = "github.com/klauspost/compress"
	// maxEncodeAll bounds the memory used by single-segment and EncodeAll
	// candidates, which must hold the whole input.
	maxEncodeAll = 1 << 30
)

// Engine produces zstd frames with klauspost/compress/zstd.
type Engine struct{}

// New returns the engine.
func New() *Engine { return &Engine{} }

func (*Engine) Name() string          { return "klauspost-zstd" }
func (*Engine) Version() string       { return engine.ModuleVersion(modulePath) }
func (*Engine) Format() engine.Format { return engine.FormatZstd }

// levelOrder lists klauspost levels: 2 default, 1 fastest, 3 better, 4 best.
var levelOrder = []int{2, 1, 3, 4}

// Candidates returns one tier. Window size, checksum and content size come
// from the header. A window size that is not a power of two cannot come
// from klauspost, so no candidates are returned.
func (*Engine) Candidates(h *format.ZstdFrameHeader, _ int64) [][]engine.ZstdParams {
	if h.WindowLog > 0 && h.WindowSize != 1<<h.WindowLog {
		return nil
	}
	var tier []engine.ZstdParams
	for _, l := range levelOrder {
		p := engine.ZstdParams{
			Level:         l,
			WindowLog:     h.WindowLog,
			Checksum:      h.Checksum,
			ContentSize:   h.HasContentSize,
			PledgedSize:   h.HasContentSize,
			SingleSegment: h.SingleSegment,
		}
		tier = append(tier, p)
		if h.HasContentSize && !h.SingleSegment {
			q := p
			q.EncodeAll = true
			tier = append(tier, q)
		}
	}
	return [][]engine.ZstdParams{tier}
}

func (*Engine) NewWriter(w io.Writer, p engine.ZstdParams, uncompressedSize int64) (io.WriteCloser, error) {
	if p.Level < 1 || p.Level > 4 {
		return nil, fmt.Errorf("klauspost-zstd: level %d out of range 1..4", p.Level)
	}
	if p.Long || p.Workers != 0 {
		return nil, errors.New("klauspost-zstd: long and workers are not supported")
	}
	opts := []zstd.EOption{
		zstd.WithEncoderLevel(zstd.EncoderLevel(p.Level)),
		zstd.WithEncoderCRC(p.Checksum),
		zstd.WithEncoderConcurrency(1),
	}
	if p.WindowLog != 0 {
		opts = append(opts, zstd.WithWindowSize(1<<p.WindowLog))
	}
	if uncompressedSize == 0 {
		opts = append(opts, zstd.WithZeroFrames(true))
	}
	if p.SingleSegment || p.EncodeAll {
		if uncompressedSize > maxEncodeAll {
			return nil, fmt.Errorf("klauspost-zstd: input of %d bytes is too large for one-shot encoding", uncompressedSize)
		}
		opts = append(opts, zstd.WithSingleSegment(p.SingleSegment))
		enc, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			return nil, err
		}
		return &allWriter{w: w, enc: enc, buf: make([]byte, 0, uncompressedSize)}, nil
	}
	enc, err := zstd.NewWriter(w, opts...)
	if err != nil {
		return nil, err
	}
	if p.ContentSize {
		enc.ResetContentSize(w, uncompressedSize)
	}
	return enc, nil
}

// allWriter buffers the input and encodes it in one EncodeAll call on Close.
type allWriter struct {
	w   io.Writer
	enc *zstd.Encoder
	buf []byte
}

func (a *allWriter) Write(p []byte) (int, error) {
	a.buf = append(a.buf, p...)
	return len(p), nil
}

func (a *allWriter) Close() error {
	defer a.enc.Close()
	_, err := a.w.Write(a.enc.EncodeAll(a.buf, nil))
	return err
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test ./engine/kpzstd/`
Expected: PASS. If `TestConcurrencyDoesNotChangeOutput` fails, stop and report; do not weaken the test. If `TestCandidatesFromEncodeAllHeader` fails because klauspost did not set single segment for a 1000-byte input, print the header and report; the candidate logic keys off the header so it may need `SingleSegment` false with `EncodeAll` true as the first candidate instead.

- [ ] **Step 5: Commit**

```bash
git add engine/kpzstd
git commit -m "Add klauspost-zstd engine"
```

---

### Task 12: root package: errors, Params JSON, engine set, helpers

**Files:**
- Create: `format.go`, `errors.go`, `params.go`, `engines.go`, `engines_cgo.go`, `engines_nocgo.go`, `io.go`, `params_test.go`, `engines_test.go`
- Modify: `go.mod` (adds blake3)

**Interfaces:**
- Consumes: `engine.*`, `format.*`, all five engine packages
- Produces:
  - `type Format = format.Format`; `const FormatNone, FormatGzip, FormatZstd`; `type GzipParams = engine.GzipParams`; `type ZstdParams = engine.ZstdParams`; `type DeflateParams = engine.DeflateParams`
  - `func Detect(r io.ReadSeeker) (Format, error)`
  - `var ErrUnsupported, ErrCorrupt, ErrNotReproducible, ErrInputMismatch, ErrDigestMismatch, ErrEngineUnavailable, ErrEngineVersionMismatch, ErrParamsVersion, ErrInvalidParams error`
  - `const ParamsVersion = 1`
  - `type Digest struct { Blake3 string json:"blake3"; Size int64 json:"size" }`
  - `type Params struct { Version int; Format Format; Compressed Digest; Uncompressed Digest; Engine string; EngineVersion string; Gzip *GzipParams; Zstd *ZstdParams }` with json tags `version`, `format`, `compressed`, `uncompressed`, `engine,omitempty`, `engine_version,omitempty`, `gzip,omitempty`, `zstd,omitempty`
  - `func (p *Params) Write(w io.Writer) error`; `func ReadParams(r io.Reader) (*Params, error)`; `func (p *Params) validate() error`
  - `func DefaultEngines() []engine.Engine` in order zlib, libzstd, go-flate, klauspost-flate, klauspost-zstd (cgo engines omitted without cgo)
  - `io.go`: `newHasher() *blake3.Hasher` (32-byte output), `digestOf(h *blake3.Hasher, n int64) Digest`, `type countingReader struct { r io.Reader; n int64 }`, `type countingWriter struct { w io.Writer; n int64 }`, `type ctxWriter struct { ctx context.Context; w io.Writer }`

- [ ] **Step 1: Add the blake3 dependency**

Run: `nix develop --command go get lukechampine.com/blake3@v1.4.1`

- [ ] **Step 2: Write failing tests**

`params_test.go`:
```go
package compprysm

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const goodGzipParams = `{
  "version": 1,
  "format": "gzip",
  "compressed": {"blake3": "` + zeroHash + `", "size": 10},
  "uncompressed": {"blake3": "` + zeroHash + `", "size": 20},
  "engine": "zlib",
  "engine_version": "1.3.2",
  "gzip": {"header_b64": "H4sIAAAAAAAAAw==", "level": 6, "strategy": "default", "window_bits": 15, "mem_level": 8}
}`

const zeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

func TestReadParamsRoundTrip(t *testing.T) {
	p, err := ReadParams(strings.NewReader(goodGzipParams))
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != "zlib" || p.Gzip == nil || p.Gzip.Level != 6 || p.Gzip.MemLevel != 8 || p.Compressed.Size != 10 {
		t.Fatalf("%+v", p)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	back, err := ReadParams(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if *back.Gzip != *p.Gzip || back.Compressed != p.Compressed || back.Engine != p.Engine {
		t.Fatalf("round trip differs: %+v", back)
	}
	if !strings.Contains(buf.String(), "\n  \"format\": \"gzip\"") {
		t.Fatalf("output is not indented: %s", buf.String())
	}
}

func TestReadParamsRejectsVersion(t *testing.T) {
	_, err := ReadParams(strings.NewReader(strings.Replace(goodGzipParams, `"version": 1`, `"version": 2`, 1)))
	if !errors.Is(err, ErrParamsVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestReadParamsValidation(t *testing.T) {
	cases := map[string]string{
		"missing gzip section": strings.Replace(goodGzipParams, `"gzip":`, `"nope":`, 1),
		"bad format":           strings.Replace(goodGzipParams, `"gzip",`, `"lzma",`, 1),
		"short hash":           strings.Replace(goodGzipParams, zeroHash+`", "size": 10`, `abc", "size": 10`, 1),
		"bad header base64":    strings.Replace(goodGzipParams, `H4sIAAAAAAAAAw==`, `not base64!`, 1),
		"missing engine":       strings.Replace(goodGzipParams, `"engine": "zlib",`, ``, 1),
		"negative size":        strings.Replace(goodGzipParams, `"size": 20`, `"size": -1`, 1),
	}
	for name, in := range cases {
		_, err := ReadParams(strings.NewReader(in))
		if !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

func TestReadParamsNone(t *testing.T) {
	in := `{"version":1,"format":"none","compressed":{"blake3":"` + zeroHash + `","size":5},"uncompressed":{"blake3":"` + zeroHash + `","size":5}}`
	p, err := ReadParams(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if p.Format != FormatNone || p.Engine != "" {
		t.Fatalf("%+v", p)
	}
	bad := strings.Replace(in, `"format":"none"`, `"format":"none","engine":"zlib"`, 1)
	if _, err := ReadParams(strings.NewReader(bad)); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("engine on none: %v", err)
	}
}
```

`engines_test.go`:
```go
package compprysm

import "testing"

func TestDefaultEnginesOrderAndNames(t *testing.T) {
	var names []string
	for _, e := range DefaultEngines() {
		names = append(names, e.Name())
		if e.Version() == "" {
			t.Errorf("%s has empty version", e.Name())
		}
	}
	want := []string{"zlib", "libzstd", "go-flate", "klauspost-flate", "klauspost-zstd"}
	if len(names) != len(want) {
		t.Fatalf("got %v", names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v want %v", names, want)
		}
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `nix develop --command go test .` → build failure.

- [ ] **Step 4: Implement**

`format.go`:
```go
// Package compprysm makes compressed files reproducible from their
// uncompressed content: Analyze finds the engine and parameters that
// re-create a gzip or zstd file exactly, and Recompress rebuilds it.
package compprysm

import (
	"io"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
)

// Format identifies a compression container.
type Format = format.Format

const (
	FormatNone = format.None
	FormatGzip = format.Gzip
	FormatZstd = format.Zstd
)

// Parameter types are defined in package engine and re-exported here.
type (
	GzipParams    = engine.GzipParams
	ZstdParams    = engine.ZstdParams
	DeflateParams = engine.DeflateParams
)

// Detect reads the magic bytes and seeks r back to its start. An input
// shorter than any magic sequence is FormatNone.
func Detect(r io.ReadSeeker) (Format, error) { return format.Detect(r) }
```

`errors.go`:
```go
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
```

`params.go`:
```go
package compprysm

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// ParamsVersion is the schema version written by this package.
const ParamsVersion = 1

// Digest identifies content by blake3 hash and size.
type Digest struct {
	Blake3 string `json:"blake3"` // 64 hex characters
	Size   int64  `json:"size"`
}

// Params records how to rebuild a compressed file from its content.
type Params struct {
	Version       int         `json:"version"`
	Format        Format      `json:"format"`
	Compressed    Digest      `json:"compressed"`
	Uncompressed  Digest      `json:"uncompressed"`
	Engine        string      `json:"engine,omitempty"`
	EngineVersion string      `json:"engine_version,omitempty"`
	Gzip          *GzipParams `json:"gzip,omitempty"`
	Zstd          *ZstdParams `json:"zstd,omitempty"`
}

// Write encodes p as indented JSON.
func (p *Params) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

// ReadParams decodes and validates a Params document.
func ReadParams(r io.Reader) (*Params, error) {
	var p Params
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("compprysm: decode params: %w", err)
	}
	if p.Version != ParamsVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrParamsVersion, p.Version, ParamsVersion)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Params) validate() error {
	for name, d := range map[string]Digest{"compressed": p.Compressed, "uncompressed": p.Uncompressed} {
		if b, err := hex.DecodeString(d.Blake3); err != nil || len(b) != 32 {
			return fmt.Errorf("%w: %s blake3 must be 64 hex characters", ErrInvalidParams, name)
		}
		if d.Size < 0 {
			return fmt.Errorf("%w: %s size is negative", ErrInvalidParams, name)
		}
	}
	switch p.Format {
	case FormatNone:
		if p.Engine != "" || p.Gzip != nil || p.Zstd != nil {
			return fmt.Errorf("%w: format none must not carry an engine or parameters", ErrInvalidParams)
		}
	case FormatGzip:
		if p.Engine == "" || p.Gzip == nil || p.Zstd != nil {
			return fmt.Errorf("%w: gzip needs engine and gzip section only", ErrInvalidParams)
		}
		if _, err := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64); err != nil {
			return fmt.Errorf("%w: gzip header_b64: %v", ErrInvalidParams, err)
		}
	case FormatZstd:
		if p.Engine == "" || p.Zstd == nil || p.Gzip != nil {
			return fmt.Errorf("%w: zstd needs engine and zstd section only", ErrInvalidParams)
		}
	default:
		return fmt.Errorf("%w: unknown format %q", ErrInvalidParams, p.Format)
	}
	return nil
}
```

`engines.go`:
```go
package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
)

// DefaultEngines returns every engine compiled into the binary, most likely
// producers of files from the wild first. The cgo engines zlib and libzstd
// are present only when built with cgo.
func DefaultEngines() []engine.Engine {
	return append(cgoEngines(), goflate.New(), kpflate.New(), kpzstd.New())
}
```

`engines_cgo.go`:
```go
//go:build cgo

package compprysm

import (
	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/zlib"
)

func cgoEngines() []engine.Engine {
	return []engine.Engine{zlib.New(), libzstd.New()}
}
```

`engines_nocgo.go`:
```go
//go:build !cgo

package compprysm

import "github.com/draganm/comp-prysm/engine"

func cgoEngines() []engine.Engine { return nil }
```

`io.go`:
```go
package compprysm

import (
	"context"
	"encoding/hex"
	"io"

	"lukechampine.com/blake3"
)

func newHasher() *blake3.Hasher { return blake3.New(32, nil) }

func digestOf(h *blake3.Hasher, n int64) Digest {
	return Digest{Blake3: hex.EncodeToString(h.Sum(nil)), Size: n}
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// ctxWriter fails writes once ctx is done, so engines that do not take a
// context still stop within one write.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix develop --command go test . && nix develop --command go vet ./...`
Expected: PASS, vet clean. `io.go` helpers are unused until Task 13; if vet or the compiler complains about unused unexported identifiers it will not (Go allows unused functions), but `go vet` may not either. Proceed.

- [ ] **Step 6: Commit**

```bash
git add format.go errors.go params.go engines.go engines_cgo.go engines_nocgo.go io.go params_test.go engines_test.go go.mod go.sum
git commit -m "Add root package types, params JSON and default engine set"
```

---

### Task 13: Analyze

**Files:**
- Create: `analyze.go`, `analyze_test.go`

**Interfaces:**
- Consumes: `search.Run`, `search.Input`, `search.NewSpool`, `search.ErrNoMatch`, `format.ParseGzipHeader`, `format.ZstdFrameLength`, `format.ErrSkippableFrame`, engines, `enginetest.Gzip`, `enginetest.Zstd`, `fixtures.All`
- Produces:
  - `type Options struct { TempDir string; MaxInMemory int64; Parallelism int; Uncompressed io.Writer; Engines []engine.Engine }`
  - `const DefaultMaxInMemory = 64 << 20`
  - `func Analyze(ctx context.Context, r io.ReadSeeker, opts *Options) (*Params, error)`

- [ ] **Step 1: Write failing tests**

`analyze_test.go`:
```go
package compprysm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/zlib"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

// seekOnly hides io.ReaderAt so the sequential search path is exercised.
// The reader is a named field, not embedded, so ReadAt is not promoted.
type seekOnly struct{ r *bytes.Reader }

func (s seekOnly) Read(p []byte) (int, error)                 { return s.r.Read(p) }
func (s seekOnly) Seek(off int64, whence int) (int64, error) { return s.r.Seek(off, whence) }

func analyze(t *testing.T, file []byte, opts *Options) *Params {
	t.Helper()
	p, err := Analyze(context.Background(), bytes.NewReader(file), opts)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return p
}

func TestAnalyzeNone(t *testing.T) {
	data := fixtures.Text(1000)
	var unc bytes.Buffer
	p := analyze(t, data, &Options{Uncompressed: &unc})
	if p.Format != FormatNone || p.Engine != "" || p.Compressed != p.Uncompressed || p.Compressed.Size != 1000 {
		t.Fatalf("%+v", p)
	}
	if !bytes.Equal(unc.Bytes(), data) {
		t.Fatal("uncompressed writer did not receive the content")
	}
	if p.Version != ParamsVersion {
		t.Fatalf("version %d", p.Version)
	}
}

func TestAnalyzeEmptyInput(t *testing.T) {
	p := analyze(t, nil, nil)
	if p.Format != FormatNone || p.Compressed.Size != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestAnalyzeGzipFindsProducer(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []struct {
		engine engine.DeflateEngine
		params engine.DeflateParams
	}{
		{zlib.New(), engine.DeflateParams{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8}},
		{zlib.New(), engine.DeflateParams{Level: 9, Strategy: "default", WindowBits: 15, MemLevel: 8}},
		{zlib.New(), engine.DeflateParams{Level: 4, Strategy: "filtered", WindowBits: 15, MemLevel: 8}},
		{goflate.New(), engine.DeflateParams{Level: 6}},
		{goflate.New(), engine.DeflateParams{Level: 1}},
	} {
		file := enginetest.Gzip(t, tc.engine, tc.params, data)
		var unc bytes.Buffer
		p := analyze(t, file, &Options{Uncompressed: &unc})
		if p.Format != FormatGzip || p.Gzip == nil {
			t.Fatalf("%+v", p)
		}
		if !bytes.Equal(unc.Bytes(), data) || p.Uncompressed.Size != int64(len(data)) || p.Compressed.Size != int64(len(file)) {
			t.Fatalf("sizes/content wrong: %+v", p)
		}
		hdr, _ := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64)
		if !bytes.Equal(hdr, file[:10]) {
			t.Fatalf("header %x", hdr)
		}
		// The found candidate must reproduce the file, whichever engine it names.
		e, _ := engine.ByName(DefaultEngines(), p.Engine)
		again := enginetest.Gzip(t, e.(engine.DeflateEngine), p.Gzip.DeflateParams, data)
		if !bytes.Equal(again, file) {
			t.Fatalf("%s %+v does not reproduce a file made by %s %+v", p.Engine, p.Gzip.DeflateParams, tc.engine.Name(), tc.params)
		}
		t.Logf("%s %+v -> %s %s %+v", tc.engine.Name(), tc.params, p.Engine, p.EngineVersion, p.Gzip.DeflateParams)
	}
}

func TestAnalyzeZstdFindsProducer(t *testing.T) {
	data := fixtures.Mixed(300 << 10)
	for _, tc := range []struct {
		engine engine.ZstdEngine
		params engine.ZstdParams
	}{
		{libzstd.New(), engine.ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true}},
		{libzstd.New(), engine.ZstdParams{Level: 12, Workers: 1}},
		{kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}},
		{kpzstd.New(), engine.ZstdParams{Level: 3, ContentSize: true, PledgedSize: true, SingleSegment: true}},
	} {
		file := enginetest.Zstd(t, tc.engine, tc.params, data)
		p := analyze(t, file, nil)
		if p.Format != FormatZstd || p.Zstd == nil {
			t.Fatalf("%+v", p)
		}
		e, _ := engine.ByName(DefaultEngines(), p.Engine)
		again := enginetest.Zstd(t, e.(engine.ZstdEngine), *p.Zstd, data)
		if !bytes.Equal(again, file) {
			t.Fatalf("%s %+v does not reproduce a file made by %s %+v", p.Engine, *p.Zstd, tc.engine.Name(), tc.params)
		}
		t.Logf("%s %+v -> %s %s %+v", tc.engine.Name(), tc.params, p.Engine, p.EngineVersion, *p.Zstd)
	}
}

func TestAnalyzeSequentialAndSpilled(t *testing.T) {
	data := fixtures.Text(200 << 10)
	file := enginetest.Gzip(t, zlib.New(), engine.DeflateParams{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8}, data)
	p, err := Analyze(context.Background(), seekOnly{r: bytes.NewReader(file)}, &Options{MaxInMemory: 1, TempDir: t.TempDir(), Parallelism: 8})
	if err != nil {
		t.Fatal(err)
	}
	if p.Gzip == nil || p.Gzip.Level != 6 {
		t.Fatalf("%+v", p)
	}
}

func TestAnalyzeUnsupported(t *testing.T) {
	data := fixtures.Text(1000)
	one := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	multi := append(append([]byte{}, one...), one...)
	if _, err := Analyze(context.Background(), bytes.NewReader(multi), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("multi-member: %v", err)
	}
	z := enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, data)
	multiZ := append(append([]byte{}, z...), z...)
	if _, err := Analyze(context.Background(), bytes.NewReader(multiZ), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("multi-frame: %v", err)
	}
	dict := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x02, 0x40, 0x34, 0x12, 0x01, 0x00, 0x00}
	if _, err := Analyze(context.Background(), bytes.NewReader(dict), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("dictionary: %v", err)
	}
	skip := []byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}
	if _, err := Analyze(context.Background(), bytes.NewReader(skip), nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("skippable: %v", err)
	}
}

func TestAnalyzeCorrupt(t *testing.T) {
	data := fixtures.Text(10000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	truncated := file[:len(file)-20]
	if _, err := Analyze(context.Background(), bytes.NewReader(truncated), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("truncated: %v", err)
	}
	badTrailer := append([]byte{}, file...)
	badTrailer[len(badTrailer)-1] ^= 0xff
	if _, err := Analyze(context.Background(), bytes.NewReader(badTrailer), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad trailer: %v", err)
	}
	z := enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, data)
	z[len(z)-1] ^= 0xff
	if _, err := Analyze(context.Background(), bytes.NewReader(z), nil); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad zstd checksum: %v", err)
	}
}

func TestAnalyzeNotReproducible(t *testing.T) {
	data := fixtures.Text(10000)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	// Only zstd engines offered: no deflate candidates at all.
	_, err := Analyze(context.Background(), bytes.NewReader(file), &Options{Engines: []engine.Engine{kpzstd.New()}})
	if !errors.Is(err, ErrNotReproducible) {
		t.Fatalf("got %v", err)
	}
}

func TestAnalyzeCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, fixtures.Text(10000))
	if _, err := Analyze(ctx, bytes.NewReader(file), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

var _ io.ReadSeeker = seekOnly{}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test . -run Analyze` → build failure.

- [ ] **Step 3: Implement**

`analyze.go`:
```go
package compprysm

import (
	"bufio"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/format"
	"github.com/draganm/comp-prysm/search"
)

// DefaultMaxInMemory is the spool size above which content goes to a temp file.
const DefaultMaxInMemory = 64 << 20

// Options configures Analyze. The zero value uses the defaults.
type Options struct {
	// TempDir holds the spool for large inputs. Default os.TempDir().
	TempDir string
	// MaxInMemory is the spool size kept in memory. Default DefaultMaxInMemory.
	MaxInMemory int64
	// Parallelism is the number of candidates evaluated at once. Default
	// runtime.NumCPU(). Takes effect only when r implements io.ReaderAt.
	Parallelism int
	// Uncompressed, if set, receives the decompressed content during the
	// first pass.
	Uncompressed io.Writer
	// Engines to search. Default DefaultEngines().
	Engines []engine.Engine
}

func (o *Options) withDefaults() *Options {
	out := Options{}
	if o != nil {
		out = *o
	}
	if out.TempDir == "" {
		out.TempDir = os.TempDir()
	}
	if out.MaxInMemory <= 0 {
		out.MaxInMemory = DefaultMaxInMemory
	}
	if out.Parallelism <= 0 {
		out.Parallelism = runtime.NumCPU()
	}
	if out.Engines == nil {
		out.Engines = DefaultEngines()
	}
	return &out
}

// Analyze detects the format of r, decompresses it once while hashing both
// streams, and searches for an engine and parameters that reproduce r
// exactly. For an uncompressed input it returns Params with FormatNone.
func Analyze(ctx context.Context, r io.ReadSeeker, opts *Options) (*Params, error) {
	o := opts.withDefaults()
	f, err := format.Detect(r)
	if err != nil {
		return nil, err
	}
	switch f {
	case FormatGzip:
		return analyzeGzip(ctx, r, o)
	case FormatZstd:
		return analyzeZstd(ctx, r, o)
	default:
		return analyzeNone(r, o)
	}
}

func analyzeNone(r io.Reader, o *Options) (*Params, error) {
	h := newHasher()
	w := io.Writer(h)
	if o.Uncompressed != nil {
		w = io.MultiWriter(h, o.Uncompressed)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return nil, err
	}
	d := digestOf(h, n)
	return &Params{Version: ParamsVersion, Format: FormatNone, Compressed: d, Uncompressed: d}, nil
}

// payloadSource returns a factory for readers over r from off to size, and
// whether those readers may be used concurrently.
func payloadSource(r io.ReadSeeker, off, size int64) (func() (io.Reader, error), bool) {
	if ra, ok := r.(io.ReaderAt); ok {
		return func() (io.Reader, error) { return io.NewSectionReader(ra, off, size-off), nil }, true
	}
	return func() (io.Reader, error) {
		if _, err := r.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		return r, nil
	}, false
}

// spoolWriters builds the fan-out for the decompressed stream.
func spoolWriters(o *Options, sp *search.Spool, extra ...io.Writer) io.Writer {
	ws := append([]io.Writer{sp}, extra...)
	if o.Uncompressed != nil {
		ws = append(ws, o.Uncompressed)
	}
	return io.MultiWriter(ws...)
}

func analyzeGzip(ctx context.Context, r io.ReadSeeker, o *Options) (*Params, error) {
	compHash := newHasher()
	cr := &countingReader{r: io.TeeReader(r, compHash)}
	br := bufio.NewReaderSize(cr, 64<<10)
	hdr, err := format.ParseGzipHeader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	sp := search.NewSpool(o.TempDir, o.MaxInMemory)
	defer sp.Close()
	uncHash := newHasher()
	crc := crc32.NewIEEE()
	fr := flate.NewReader(br)
	n, err := io.Copy(spoolWriters(o, sp, uncHash, crc), fr)
	if err != nil {
		return nil, fmt.Errorf("%w: deflate: %v", ErrCorrupt, err)
	}
	fr.Close()
	var trailer [8]byte
	if _, err := io.ReadFull(br, trailer[:]); err != nil {
		return nil, fmt.Errorf("%w: gzip trailer: %v", ErrCorrupt, err)
	}
	if binary.LittleEndian.Uint32(trailer[0:4]) != crc.Sum32() || binary.LittleEndian.Uint32(trailer[4:8]) != uint32(n) {
		return nil, fmt.Errorf("%w: gzip trailer does not match content", ErrCorrupt)
	}
	extra, err := io.Copy(io.Discard, br)
	if err != nil {
		return nil, err
	}
	if extra > 0 {
		return nil, fmt.Errorf("%w: multi-member gzip (%d bytes after the first member)", ErrUnsupported, extra)
	}
	in := &search.Input{Format: FormatGzip, Trailer: trailer[:], Spool: sp, UncompressedSize: n}
	in.Payload, in.Concurrent = payloadSource(r, int64(len(hdr.Raw)), cr.n)
	res, err := runSearch(ctx, in, gzipCandidates(o.Engines, hdr, n), o.Parallelism)
	if err != nil {
		return nil, err
	}
	return &Params{
		Version:       ParamsVersion,
		Format:        FormatGzip,
		Compressed:    digestOf(compHash, cr.n),
		Uncompressed:  digestOf(uncHash, n),
		Engine:        res.Candidate.Engine.Name(),
		EngineVersion: res.Candidate.Engine.Version(),
		Gzip:          &GzipParams{HeaderB64: base64.StdEncoding.EncodeToString(hdr.Raw), DeflateParams: *res.Candidate.Deflate},
	}, nil
}

func analyzeZstd(ctx context.Context, r io.ReadSeeker, o *Options) (*Params, error) {
	// Pass 0: find the frame extent without decoding so the decoder can be
	// bounded to exactly one frame.
	hdr, frameLen, err := format.ZstdFrameLength(bufio.NewReaderSize(r, 64<<10))
	if err != nil {
		if errors.Is(err, format.ErrSkippableFrame) {
			return nil, fmt.Errorf("%w: zstd skippable frame", ErrUnsupported)
		}
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if hdr.DictID != 0 {
		return nil, fmt.Errorf("%w: zstd dictionary %d", ErrUnsupported, hdr.DictID)
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	compHash := newHasher()
	cr := &countingReader{r: io.TeeReader(r, compHash)}
	br := bufio.NewReaderSize(cr, 64<<10)
	dec, err := zstd.NewReader(io.LimitReader(br, frameLen), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	sp := search.NewSpool(o.TempDir, o.MaxInMemory)
	defer sp.Close()
	uncHash := newHasher()
	n, err := io.Copy(spoolWriters(o, sp, uncHash), dec)
	if err != nil {
		return nil, fmt.Errorf("%w: zstd: %v", ErrCorrupt, err)
	}
	if hdr.HasContentSize && hdr.ContentSize != uint64(n) {
		return nil, fmt.Errorf("%w: zstd content size %d, decoded %d", ErrCorrupt, hdr.ContentSize, n)
	}
	extra, err := io.Copy(io.Discard, br)
	if err != nil {
		return nil, err
	}
	if extra > 0 {
		return nil, fmt.Errorf("%w: multi-frame zstd (%d bytes after the first frame)", ErrUnsupported, extra)
	}
	in := &search.Input{Format: FormatZstd, Spool: sp, UncompressedSize: n}
	in.Payload, in.Concurrent = payloadSource(r, 0, cr.n)
	res, err := runSearch(ctx, in, zstdCandidates(o.Engines, hdr, n), o.Parallelism)
	if err != nil {
		return nil, err
	}
	return &Params{
		Version:       ParamsVersion,
		Format:        FormatZstd,
		Compressed:    digestOf(compHash, cr.n),
		Uncompressed:  digestOf(uncHash, n),
		Engine:        res.Candidate.Engine.Name(),
		EngineVersion: res.Candidate.Engine.Version(),
		Zstd:          res.Candidate.Zstd,
	}, nil
}

func runSearch(ctx context.Context, in *search.Input, cands []search.Candidate, parallelism int) (*search.Result, error) {
	res, err := search.Run(ctx, in, cands, parallelism)
	if err != nil {
		if errors.Is(err, search.ErrNoMatch) {
			return nil, fmt.Errorf("%w: %v", ErrNotReproducible, err)
		}
		return nil, err
	}
	return res, nil
}

func gzipCandidates(engines []engine.Engine, h *format.GzipHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate // tiers[i] holds every engine's tier i, in engine order
	for _, e := range engines {
		de, ok := e.(engine.DeflateEngine)
		if !ok {
			continue
		}
		for i, tier := range de.Candidates(h, size) {
			for len(tiers) <= i {
				tiers = append(tiers, nil)
			}
			for _, p := range tier {
				p := p
				tiers[i] = append(tiers[i], search.Candidate{Engine: e, Deflate: &p})
			}
		}
	}
	return flatten(tiers)
}

func zstdCandidates(engines []engine.Engine, h *format.ZstdFrameHeader, size int64) []search.Candidate {
	var tiers [][]search.Candidate
	for _, e := range engines {
		ze, ok := e.(engine.ZstdEngine)
		if !ok {
			continue
		}
		for i, tier := range ze.Candidates(h, size) {
			for len(tiers) <= i {
				tiers = append(tiers, nil)
			}
			for _, p := range tier {
				p := p
				tiers[i] = append(tiers[i], search.Candidate{Engine: e, Zstd: &p})
			}
		}
	}
	return flatten(tiers)
}

func flatten(tiers [][]search.Candidate) []search.Candidate {
	var out []search.Candidate
	for _, t := range tiers {
		out = append(out, t...)
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test -race . -run Analyze -v`
Expected: PASS. Watch the `t.Logf` lines: a file made by go-flate should be found as go-flate, and zlib as zlib, unless two engines produce identical bytes for that input, in which case the first in engine order wins and the test still passes because it checks reproduction.

- [ ] **Step 5: Commit**

```bash
git add analyze.go analyze_test.go
git commit -m "Add Analyze: single pass decompress, hash, spool and search"
```

---

### Task 14: Recompress

**Files:**
- Create: `recompress.go`, `recompress_test.go`

**Interfaces:**
- Consumes: `Params`, `engine.ByName`, `engine.DeflateEngine`, `engine.ZstdEngine`, `countingReader`, `countingWriter`, `ctxWriter`, `digestOf`, `enginetest`, `fixtures`
- Produces:
  - `type RecompressOptions struct { Engines []engine.Engine; AllowVersionMismatch bool }`
  - `func Recompress(ctx context.Context, p *Params, uncompressed io.Reader, w io.Writer, opts *RecompressOptions) error`

- [ ] **Step 1: Write failing tests**

`recompress_test.go`:
```go
package compprysm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/engine/kpzstd"
	"github.com/draganm/comp-prysm/engine/libzstd"
	"github.com/draganm/comp-prysm/engine/zlib"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

func recompress(t *testing.T, p *Params, data []byte, opts *RecompressOptions) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := Recompress(context.Background(), p, bytes.NewReader(data), &out, opts)
	return out.Bytes(), err
}

func TestRecompressRoundTrip(t *testing.T) {
	for _, f := range fixtures.All() {
		files := map[string][]byte{
			"zlib-6":        enginetest.Gzip(t, zlib.New(), engine.DeflateParams{Level: 6, Strategy: "default", WindowBits: 15, MemLevel: 8}, f.Data),
			"goflate-9":     enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 9}, f.Data),
			"libzstd-3":     enginetest.Zstd(t, libzstd.New(), engine.ZstdParams{Level: 3, Checksum: true, ContentSize: true, PledgedSize: true}, f.Data),
			"libzstd-7-mt":  enginetest.Zstd(t, libzstd.New(), engine.ZstdParams{Level: 7, Workers: 1}, f.Data),
			"kpzstd-stream": enginetest.Zstd(t, kpzstd.New(), engine.ZstdParams{Level: 2, Checksum: true}, f.Data),
			"none":          f.Data,
		}
		for name, file := range files {
			t.Run(f.Name+"/"+name, func(t *testing.T) {
				p := analyze(t, file, nil)
				out, err := recompress(t, p, f.Data, nil)
				if err != nil {
					t.Fatalf("recompress: %v", err)
				}
				if !bytes.Equal(out, file) {
					t.Fatal("recompressed bytes differ")
				}
			})
		}
	}
}

func TestRecompressInputMismatch(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	wrong := append([]byte{}, data...)
	wrong[500] ^= 1
	if _, err := recompress(t, p, wrong, nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("got %v", err)
	}
	if _, err := recompress(t, p, data[:9000], nil); !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("short input: %v", err)
	}
}

func TestRecompressDigestMismatch(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	p.Gzip.Level = 1
	if _, err := recompress(t, p, data, nil); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestRecompressEngineChecks(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)

	q := *p
	q.Engine = "nope"
	if _, err := recompress(t, &q, data, nil); !errors.Is(err, ErrEngineUnavailable) {
		t.Fatalf("unknown engine: %v", err)
	}

	q = *p
	q.EngineVersion = "go0.0"
	if _, err := recompress(t, &q, data, nil); !errors.Is(err, ErrEngineVersionMismatch) {
		t.Fatalf("version: %v", err)
	}
	out, err := recompress(t, &q, data, &RecompressOptions{AllowVersionMismatch: true})
	if err != nil || len(out) == 0 {
		t.Fatalf("allow mismatch: %v", err)
	}

	q = *p
	q.Engine = "klauspost-zstd"
	if _, err := recompress(t, &q, data, &RecompressOptions{AllowVersionMismatch: true}); !errors.Is(err, ErrEngineUnavailable) {
		t.Fatalf("wrong format engine: %v", err)
	}
}

func TestRecompressInvalidParams(t *testing.T) {
	p := &Params{Version: ParamsVersion, Format: FormatGzip}
	if _, err := recompress(t, p, nil, nil); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("got %v", err)
	}
	p = &Params{Version: 7, Format: FormatNone}
	if _, err := recompress(t, p, nil, nil); !errors.Is(err, ErrParamsVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestRecompressCancelled(t *testing.T) {
	data := fixtures.Text(10000)
	p := analyze(t, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Recompress(ctx, p, bytes.NewReader(data), io.Discard, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `nix develop --command go test . -run Recompress` → build failure.

- [ ] **Step 3: Implement**

`recompress.go`:
```go
package compprysm

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/draganm/comp-prysm/engine"
)

// RecompressOptions configures Recompress. The zero value uses the defaults.
type RecompressOptions struct {
	// Engines to look the recorded engine up in. Default DefaultEngines().
	Engines []engine.Engine
	// AllowVersionMismatch tries the engine even when its version differs
	// from the recorded one. The digest check still decides the outcome.
	AllowVersionMismatch bool
}

// Recompress rebuilds the compressed file described by p from uncompressed
// and streams it to w. It verifies the input against p.Uncompressed and the
// output against p.Compressed. Bytes already written to w are not rolled
// back on error; write to a temporary file and rename on success.
func Recompress(ctx context.Context, p *Params, uncompressed io.Reader, w io.Writer, opts *RecompressOptions) error {
	o := RecompressOptions{}
	if opts != nil {
		o = *opts
	}
	if o.Engines == nil {
		o.Engines = DefaultEngines()
	}
	if p.Version != ParamsVersion {
		return fmt.Errorf("%w: got %d, want %d", ErrParamsVersion, p.Version, ParamsVersion)
	}
	if err := p.validate(); err != nil {
		return err
	}
	inHash := newHasher()
	in := &countingReader{r: io.TeeReader(uncompressed, inHash)}
	outHash := newHasher()
	out := &countingWriter{w: &ctxWriter{ctx: ctx, w: io.MultiWriter(w, outHash)}}

	var err error
	switch p.Format {
	case FormatNone:
		_, err = io.Copy(out, in)
	case FormatGzip:
		err = recompressGzip(p, in, out, &o)
	case FormatZstd:
		err = recompressZstd(p, in, out, &o)
	}
	if err != nil {
		return err
	}
	if got := digestOf(inHash, in.n); got != p.Uncompressed {
		return fmt.Errorf("%w: input is %s/%d, params expect %s/%d", ErrInputMismatch, got.Blake3, got.Size, p.Uncompressed.Blake3, p.Uncompressed.Size)
	}
	if got := digestOf(outHash, out.n); got != p.Compressed {
		return fmt.Errorf("%w: output is %s/%d, params expect %s/%d", ErrDigestMismatch, got.Blake3, got.Size, p.Compressed.Blake3, p.Compressed.Size)
	}
	return nil
}

func lookupEngine(p *Params, o *RecompressOptions) (engine.Engine, error) {
	e, ok := engine.ByName(o.Engines, p.Engine)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrEngineUnavailable, p.Engine)
	}
	if e.Format() != p.Format {
		return nil, fmt.Errorf("%w: %q produces %s, params are %s", ErrEngineUnavailable, p.Engine, e.Format(), p.Format)
	}
	if v := e.Version(); v != p.EngineVersion && !o.AllowVersionMismatch {
		return nil, fmt.Errorf("%w: %s is %s, params were made with %s", ErrEngineVersionMismatch, p.Engine, v, p.EngineVersion)
	}
	return e, nil
}

func recompressGzip(p *Params, in *countingReader, out io.Writer, o *RecompressOptions) error {
	e, err := lookupEngine(p, o)
	if err != nil {
		return err
	}
	de := e.(engine.DeflateEngine)
	hdr, err := base64.StdEncoding.DecodeString(p.Gzip.HeaderB64)
	if err != nil {
		return fmt.Errorf("%w: gzip header_b64: %v", ErrInvalidParams, err)
	}
	if _, err := out.Write(hdr); err != nil {
		return err
	}
	crc := crc32.NewIEEE()
	dw, err := de.NewWriter(out, p.Gzip.DeflateParams)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dw, io.TeeReader(in, crc)); err != nil {
		dw.Close()
		return err
	}
	if err := dw.Close(); err != nil {
		return err
	}
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[0:4], crc.Sum32())
	binary.LittleEndian.PutUint32(trailer[4:8], uint32(in.n))
	_, err = out.Write(trailer[:])
	return err
}

func recompressZstd(p *Params, in io.Reader, out io.Writer, o *RecompressOptions) error {
	e, err := lookupEngine(p, o)
	if err != nil {
		return err
	}
	zw, err := e.(engine.ZstdEngine).NewWriter(out, *p.Zstd, p.Uncompressed.Size)
	if err != nil {
		return err
	}
	if _, err := io.Copy(zw, in); err != nil {
		zw.Close()
		return err
	}
	return zw.Close()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix develop --command go test -race . -v -run Recompress`
Expected: PASS. `TestRecompressRoundTrip` runs 11 fixtures times 6 files; expect roughly a minute.

- [ ] **Step 5: Commit**

```bash
git add recompress.go recompress_test.go
git commit -m "Add Recompress with input and output digest verification"
```

---

### Task 15: Wild fixtures from real gzip, pigz and zstd

This task is the spike from the spec's risk section. Its result decides whether a GNU gzip or pigz engine is needed. Report the outcome; do not add engines here.

**Files:**
- Create: `wild_test.go`

**Interfaces:**
- Consumes: `Analyze`, `Recompress`, `fixtures`

- [ ] **Step 1: Write the tests**

`wild_test.go`:
```go
package compprysm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/draganm/comp-prysm/fixtures"
)

func tool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not on PATH; run inside the flake dev shell", name)
	}
	return p
}

// run executes a compressor. If stdin is non-nil it is piped; otherwise
// args must name an input file and the output is read from outFile.
func run(t *testing.T, stdin []byte, outFile string, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(tool(t, name), args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, errb.String())
	}
	if outFile != "" {
		b, err := os.ReadFile(outFile)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return out.Bytes()
}

func check(t *testing.T, file, data []byte) {
	t.Helper()
	p, err := Analyze(context.Background(), bytes.NewReader(file), nil)
	if err != nil {
		if errors.Is(err, ErrNotReproducible) {
			t.Errorf("NOT REPRODUCIBLE: %v", err)
			return
		}
		t.Fatalf("Analyze: %v", err)
	}
	var out bytes.Buffer
	if err := Recompress(context.Background(), p, bytes.NewReader(data), &out, nil); err != nil {
		t.Fatalf("Recompress: %v", err)
	}
	if !bytes.Equal(out.Bytes(), file) {
		t.Fatal("recompressed bytes differ")
	}
	switch p.Format {
	case FormatGzip:
		t.Logf("reproduced by %s %s %+v", p.Engine, p.EngineVersion, p.Gzip.DeflateParams)
	case FormatZstd:
		t.Logf("reproduced by %s %s %+v", p.Engine, p.EngineVersion, *p.Zstd)
	}
}

func wildInputs() []fixtures.Fixture {
	return []fixtures.Fixture{
		{"text-1m", fixtures.Text(1 << 20)},
		{"mixed-3m", fixtures.Mixed(3 << 20)},
		{"tiny", []byte("tiny input\n")},
	}
}

func TestWildGzip(t *testing.T) {
	tool(t, "gzip")
	for _, f := range wildInputs() {
		dir := t.TempDir()
		src := filepath.Join(dir, f.Name)
		os.WriteFile(src, f.Data, 0o644)
		for level := 1; level <= 9; level++ {
			lvl := "-" + string(rune('0'+level))
			t.Run(f.Name+"/stdin"+lvl, func(t *testing.T) {
				check(t, run(t, f.Data, "", "gzip", "-c", lvl), f.Data)
			})
			t.Run(f.Name+"/file"+lvl, func(t *testing.T) {
				out := src + ".gz"
				os.Remove(out)
				run(t, nil, out, "gzip", "-k", lvl, src)
				b, _ := os.ReadFile(out)
				check(t, b, f.Data)
			})
		}
	}
}

func TestWildPigz(t *testing.T) {
	tool(t, "pigz")
	for _, f := range wildInputs() {
		for _, args := range [][]string{{"-1"}, {"-6"}, {"-9"}, {"-p1", "-6"}, {"-p4", "-6"}} {
			t.Run(f.Name+"/"+filepath.Join(args...), func(t *testing.T) {
				check(t, run(t, f.Data, "", "pigz", append([]string{"-c"}, args...)...), f.Data)
			})
		}
	}
}

func TestWildZstd(t *testing.T) {
	tool(t, "zstd")
	for _, f := range wildInputs() {
		dir := t.TempDir()
		src := filepath.Join(dir, f.Name)
		os.WriteFile(src, f.Data, 0o644)
		variants := [][]string{
			{"-1"}, {"-3"}, {"-6"}, {"-9"}, {"-12"}, {"-15"}, {"-19"},
			{"--ultra", "-22"}, {"--fast=3"},
			{"-3", "-T0"}, {"-3", "-T1"}, {"-3", "-T4"}, {"-3", "--single-thread"},
			{"-3", "--no-check"}, {"-3", "--long"}, {"-9", "--long=27"},
		}
		for _, args := range variants {
			name := filepath.Join(args...)
			t.Run(f.Name+"/stdin/"+name, func(t *testing.T) {
				check(t, run(t, f.Data, "", "zstd", append([]string{"-c", "-q"}, args...)...), f.Data)
			})
			t.Run(f.Name+"/file/"+name, func(t *testing.T) {
				out := filepath.Join(dir, "out.zst")
				os.Remove(out)
				run(t, nil, out, "zstd", append([]string{"-q", "-f", "-o", out}, append(args, src)...)...)
				b, _ := os.ReadFile(out)
				check(t, b, f.Data)
			})
		}
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `nix develop --command go test . -run Wild -v 2>&1 | tee /tmp/wild.log | grep -E '^(=== RUN|--- (PASS|FAIL)|\s+wild_test.go)' | head -300`
Expected: every subtest passes and logs the engine that reproduced it. Collect the results into a table, one row per tool and variant, with pass or NOT REPRODUCIBLE. This table is the deliverable of the task and must go in the final report. Specifically answer:
1. Does GNU gzip match zlib at every level, for stdin and file input?
2. Which pigz variants are reproducible?
3. Which zstd CLI variants are reproducible, and with which Workers value?

- [ ] **Step 3: Commit whatever the outcome**

Failing wild subtests are information, not a defect in this task. If some fail, mark those subtests with `t.Skip("known: <tool> <variant> not reproducible, see plan Task 15 report")` immediately after the failing `check` call site is identified, so `go test ./...` stays green, and list every skip in the report.

```bash
git add wild_test.go
git commit -m "Add wild fixture tests against gzip, pigz and zstd CLIs"
```

---

### Task 16: CLI

**Files:**
- Create: `cmd/comp-prysm/main.go`, `cmd/comp-prysm/main_test.go`
- Modify: `go.mod` (adds urfave/cli/v2)

**Interfaces:**
- Consumes: `compprysm.Analyze`, `Recompress`, `Detect`, `ReadParams`, `DefaultEngines`
- Produces: binary `comp-prysm` with commands `detect`, `analyze`, `recompress`, `engines`. `func newApp() *cli.App` for tests.

- [ ] **Step 1: Add the dependency**

Run: `nix develop --command go get github.com/urfave/cli/v2@v2.27.7`

- [ ] **Step 2: Write failing tests**

`cmd/comp-prysm/main_test.go`:
```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/draganm/comp-prysm/engine"
	"github.com/draganm/comp-prysm/engine/goflate"
	"github.com/draganm/comp-prysm/enginetest"
	"github.com/draganm/comp-prysm/fixtures"
)

func runApp(t *testing.T, args ...string) (string, error) {
	t.Helper()
	app := newApp()
	var out bytes.Buffer
	app.Writer = &out
	app.ErrWriter = &out
	err := app.Run(append([]string{"comp-prysm"}, args...))
	return out.String(), err
}

func TestDetect(t *testing.T) {
	dir := t.TempDir()
	gz := filepath.Join(dir, "a.gz")
	os.WriteFile(gz, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, []byte("x")), 0o644)
	out, err := runApp(t, "detect", gz)
	if err != nil || strings.TrimSpace(out) != "gzip" {
		t.Fatalf("%q %v", out, err)
	}
	plain := filepath.Join(dir, "a.txt")
	os.WriteFile(plain, []byte("x"), 0o644)
	out, _ = runApp(t, "detect", plain)
	if strings.TrimSpace(out) != "none" {
		t.Fatalf("%q", out)
	}
}

func TestAnalyzeThenRecompress(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Mixed(200 << 10)
	file := enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, file, 0o644)
	params := filepath.Join(dir, "params.json")
	unc := filepath.Join(dir, "in")
	if out, err := runApp(t, "analyze", "--params", params, "--uncompressed", unc, gz); err != nil {
		t.Fatalf("analyze: %v: %s", err, out)
	}
	got, _ := os.ReadFile(unc)
	if !bytes.Equal(got, data) {
		t.Fatal("uncompressed output differs")
	}
	pj, _ := os.ReadFile(params)
	if !strings.Contains(string(pj), `"engine": "go-flate"`) && !strings.Contains(string(pj), `"engine": "zlib"`) {
		t.Fatalf("params: %s", pj)
	}
	rebuilt := filepath.Join(dir, "out.gz")
	if out, err := runApp(t, "recompress", "--params", params, unc, rebuilt); err != nil {
		t.Fatalf("recompress: %v: %s", err, out)
	}
	back, _ := os.ReadFile(rebuilt)
	if !bytes.Equal(back, file) {
		t.Fatal("rebuilt file differs")
	}
	if _, err := os.Stat(rebuilt + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}
}

func TestAnalyzeParamsToStdout(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "a.txt")
	os.WriteFile(plain, []byte("hello"), 0o644)
	out, err := runApp(t, "analyze", plain)
	if err != nil || !strings.Contains(out, `"format": "none"`) {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRecompressFailureLeavesNoOutput(t *testing.T) {
	dir := t.TempDir()
	data := fixtures.Text(10000)
	gz := filepath.Join(dir, "in.gz")
	os.WriteFile(gz, enginetest.Gzip(t, goflate.New(), engine.DeflateParams{Level: 6}, data), 0o644)
	params := filepath.Join(dir, "params.json")
	runApp(t, "analyze", "--params", params, gz)
	wrong := filepath.Join(dir, "wrong")
	os.WriteFile(wrong, []byte("not the content"), 0o644)
	rebuilt := filepath.Join(dir, "out.gz")
	if _, err := runApp(t, "recompress", "--params", params, wrong, rebuilt); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Stat(rebuilt); err == nil {
		t.Fatal("output must not exist after failure")
	}
}

func TestEngines(t *testing.T) {
	out, err := runApp(t, "engines")
	if err != nil || !strings.Contains(out, "zlib") || !strings.Contains(out, "libzstd") {
		t.Fatalf("%q %v", out, err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `nix develop --command go test ./cmd/...` → build failure.

- [ ] **Step 4: Implement**

`cmd/comp-prysm/main.go`:
```go
// Command comp-prysm analyzes compressed files and rebuilds them from
// uncompressed content.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/urfave/cli/v2"

	compprysm "github.com/draganm/comp-prysm"
)

func main() {
	if err := newApp().Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newApp() *cli.App {
	return &cli.App{
		Name:  "comp-prysm",
		Usage: "make compressed files reproducible from their content",
		Commands: []*cli.Command{
			{
				Name:      "detect",
				Usage:     "print gzip, zstd or none for a file",
				ArgsUsage: "<file>",
				Action:    detect,
			},
			{
				Name:      "analyze",
				Usage:     "find parameters that reproduce a compressed file",
				ArgsUsage: "<file>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "params", Usage: "write params JSON to `FILE` (default stdout)"},
					&cli.StringFlag{Name: "uncompressed", Usage: "write the uncompressed content to `FILE`"},
					&cli.IntFlag{Name: "parallelism", Usage: "candidates evaluated at once (default: CPUs)"},
					&cli.StringFlag{Name: "temp-dir", Usage: "spool directory for large inputs"},
				},
				Action: analyze,
			},
			{
				Name:      "recompress",
				Usage:     "rebuild a compressed file from params and uncompressed content",
				ArgsUsage: "<uncompressed> <out>",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "params", Usage: "params JSON `FILE`", Required: true},
					&cli.BoolFlag{Name: "allow-version-mismatch", Usage: "try even if the engine version differs"},
				},
				Action: recompress,
			},
			{
				Name:   "engines",
				Usage:  "list available engines",
				Action: engines,
			},
		},
	}
}

func detect(c *cli.Context) error {
	if c.NArg() != 1 {
		return cli.Exit("usage: comp-prysm detect <file>", 1)
	}
	f, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer f.Close()
	format, err := compprysm.Detect(f)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.App.Writer, format)
	return nil
}

func analyze(c *cli.Context) error {
	if c.NArg() != 1 {
		return cli.Exit("usage: comp-prysm analyze [flags] <file>", 1)
	}
	f, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer f.Close()
	opts := &compprysm.Options{Parallelism: c.Int("parallelism"), TempDir: c.String("temp-dir")}
	if path := c.String("uncompressed"); path != "" {
		out, err := os.Create(path)
		if err != nil {
			return err
		}
		defer out.Close()
		opts.Uncompressed = out
	}
	p, err := compprysm.Analyze(context.Background(), f, opts)
	if err != nil {
		return err
	}
	var w io.Writer = c.App.Writer
	if path := c.String("params"); path != "" {
		pf, err := os.Create(path)
		if err != nil {
			return err
		}
		defer pf.Close()
		w = pf
	}
	return p.Write(w)
}

func recompress(c *cli.Context) error {
	if c.NArg() != 2 {
		return cli.Exit("usage: comp-prysm recompress --params <p.json> <uncompressed> <out>", 1)
	}
	pf, err := os.Open(c.String("params"))
	if err != nil {
		return err
	}
	p, err := compprysm.ReadParams(pf)
	pf.Close()
	if err != nil {
		return err
	}
	in, err := os.Open(c.Args().Get(0))
	if err != nil {
		return err
	}
	defer in.Close()
	outPath := c.Args().Get(1)
	tmp, err := os.CreateTemp(filepath.Dir(outPath), filepath.Base(outPath)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	opts := &compprysm.RecompressOptions{AllowVersionMismatch: c.Bool("allow-version-mismatch")}
	if err := compprysm.Recompress(context.Background(), p, in, tmp, opts); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), outPath)
}

func engines(c *cli.Context) error {
	tw := tabwriter.NewWriter(c.App.Writer, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tFORMAT\tVERSION")
	for _, e := range compprysm.DefaultEngines() {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Name(), e.Format(), e.Version())
	}
	return tw.Flush()
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix develop --command go test ./cmd/... -v` → PASS.

- [ ] **Step 6: Try the binary on a real file, then delete the binary**

```bash
nix develop --command bash -c 'go build -o /tmp/comp-prysm-bin ./cmd/comp-prysm && printf "hello hello hello\n" | gzip -9 > /tmp/cp-demo.gz && /tmp/comp-prysm-bin analyze /tmp/cp-demo.gz && /tmp/comp-prysm-bin engines; rm -f /tmp/comp-prysm-bin /tmp/cp-demo.gz'
```
Expected: params JSON on stdout naming an engine, then the engine table.

- [ ] **Step 7: Commit**

```bash
git add cmd go.mod go.sum
git commit -m "Add comp-prysm CLI"
```

---

### Task 17: Large input test, tidy, README

**Files:**
- Create: `large_test.go`, `README.md`
- Modify: `go.mod`, `go.sum` via `go mod tidy`

- [ ] **Step 1: Write the gated large test**

`large_test.go`:
```go
package compprysm

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/draganm/comp-prysm/fixtures"
)

// TestLargeInput streams a 2 GiB gzip through Analyze and Recompress. It
// runs only with COMP_PRYSM_LARGE=1 because it takes minutes and disk.
func TestLargeInput(t *testing.T) {
	if os.Getenv("COMP_PRYSM_LARGE") == "" {
		t.Skip("set COMP_PRYSM_LARGE=1 to run")
	}
	const size = 2 << 30
	dir := t.TempDir()
	unc := filepath.Join(dir, "big")
	gz := filepath.Join(dir, "big.gz")

	uf, _ := os.Create(unc)
	gf, _ := os.Create(gz)
	zw, _ := gzip.NewWriterLevel(gf, gzip.BestSpeed)
	chunk := fixtures.Mixed(4 << 20)
	w := io.MultiWriter(uf, zw)
	for written := 0; written < size; written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	gf.Close()
	uf.Close()

	in, _ := os.Open(gz)
	defer in.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	p, err := Analyze(context.Background(), in, &Options{TempDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	t.Logf("engine %s %+v; heap grew by %d MiB", p.Engine, p.Gzip.DeflateParams, (after.HeapAlloc-before.HeapAlloc)>>20)
	if p.Uncompressed.Size != size {
		t.Fatalf("size %d", p.Uncompressed.Size)
	}

	src, _ := os.Open(unc)
	defer src.Close()
	if err := Recompress(context.Background(), p, src, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
}
```

Run: `nix develop --command go test . -run LargeInput -v` → SKIP message. Then run once for real:
`nix develop --command bash -c 'COMP_PRYSM_LARGE=1 go test . -run LargeInput -v -timeout 30m'`
Expected: PASS with a log line naming go-flate level 1 (or zlib if identical) and a heap growth well under 512 MiB.

- [ ] **Step 2: Tidy and full verification**

```bash
nix develop --command go mod tidy
nix develop --command go vet ./...
nix develop --command go test -race ./...
```
Expected: `go.mod` has direct requirements for klauspost/compress, blake3 and urfave/cli only; vet clean; all tests pass.

- [ ] **Step 3: Write README.md**

Content, in this order: one-paragraph purpose; install and build inside the flake (`nix develop`, `go build ./cmd/comp-prysm`); library usage with a short Analyze then Recompress example; CLI usage for the four commands; the engine table (name, format, binding, version source); the Params JSON example from the spec; limitations: libzstd version bound, unsupported inputs (multi-member, multi-frame, skippable, dictionary), klauspost single-segment frames buffered in memory up to 1 GiB, outcome of the wild tests from Task 15 with any not-reproducible variants listed; and where the spec and plan live.

- [ ] **Step 4: Commit**

```bash
git add large_test.go README.md go.mod go.sum
git commit -m "Add large input test and README"
```

---

## Self-review notes

- Spec coverage: detection (T2), gzip header verbatim (T2, T13), zstd header and frame walk (T3), engine model and version sources (T4, T7 to T11), candidate tiers (T7 to T11), spool with spill (T5), compare writer early abort and exact EOF (T5), deterministic parallel search (T5), Params JSON and validation (T12), Analyze pass 1 with unsupported and corrupt cases (T13), Recompress with input-first checking (T14), wild fixtures and the two spikes (T15), CLI with temp-and-rename (T16), large input and README (T17), flake (T1).
- Deviation from spec: `Format` lives in `format`, `ZstdParams.EncodeAll` added; both listed under Global Constraints.
- Type consistency: `search.Input.Payload` is `func() (io.Reader, error)` in T5, T6, T13. `engine.ZstdEngine.NewWriter` takes `(w, p, uncompressedSize)` in T4, T6, T10, T11, T13, T14. `Params.validate` is unexported and used by `ReadParams` (T12) and `Recompress` (T14).
