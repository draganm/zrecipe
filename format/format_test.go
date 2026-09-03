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
