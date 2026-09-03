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
