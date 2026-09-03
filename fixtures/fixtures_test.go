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
