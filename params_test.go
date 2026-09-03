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
	text := buf.String()
	back, err := ReadParams(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	if *back.Gzip != *p.Gzip || back.Compressed != p.Compressed || back.Engine != p.Engine {
		t.Fatalf("round trip differs: %+v", back)
	}
	if !strings.Contains(text, "\n  \"format\": \"gzip\"") {
		t.Fatalf("output is not indented: %s", text)
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
