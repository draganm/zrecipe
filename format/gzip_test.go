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
